// Package claude implements runner.Runner by shelling out to the
// `claude` CLI. See ADR-0027 for the prompt-marker protocol and
// ADR-0004 for the original "shell out, not the SDK" decision.
//
// The adapter is dumb: it composes a prompt from the runner.Request
// fields (Goal, Worktree, UnitID, CommentBody, CIFailure), appends a
// decision-marker instruction, runs `claude -p --output-format json`,
// and parses the JSON envelope for token counts and the decision marker
// embedded in the result text. It does not interpret SkillCommand — the
// step body is responsible for setting Goal to a fully-formed task; this
// adapter just adds the protocol envelope claude needs to signal back.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/andrewwormald/everflow/internal/runner"
)

// claudeJSONResult is the envelope `claude -p --output-format json` writes to
// stdout. Only the fields everflow needs are unmarshalled; unknown fields are
// silently ignored.
type claudeJSONResult struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	// Result is the full text the model produced, including the
	// <everflow-decision> marker that ParseDecision reads.
	Result string `json:"result"`
	// Usage is populated by claude ≥ 1.x; older builds omit it.
	Usage *claudeUsage `json:"usage,omitempty"`
}

// claudeUsage mirrors the Anthropic API usage block embedded in the JSON
// output. Input + output token counts are the primary interest; cache
// tokens are included so the sum represents total tokens billed.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// totalTokens returns the sum of all token fields.
func (u *claudeUsage) totalTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
}

// parseJSONOutput tries to decode rawOut as a claudeJSONResult. On success it
// returns (resultText, tokens, true). On any parse failure it returns
// ("", 0, false) so the caller can fall back to treating rawOut as plain text.
func parseJSONOutput(rawOut string) (result string, tokens int, ok bool) {
	var parsed claudeJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawOut)), &parsed); err != nil {
		return "", 0, false
	}
	// Guard against an unrecognised envelope (e.g. a plain-JSON error message
	// from a wrapper script). We require type=="result" AND a non-empty Result.
	if parsed.Type != "result" || parsed.Result == "" {
		return "", 0, false
	}
	return parsed.Result, parsed.Usage.totalTokens(), true
}

// Runner is the claude.Runner. The zero value is usable (uses `claude`
// from $PATH with no extra args). NewRunner is the canonical constructor.
type Runner struct {
	// Binary is the path to the claude CLI. Defaults to "claude".
	Binary string

	// ExtraArgs is prepended to the call's args (after -p). Useful for
	// --model overrides, --debug, etc.
	ExtraArgs []string

	// Env, if non-nil, replaces os.Environ() for the subprocess. The
	// default (nil) inherits the daemon's env, which is what production
	// wants — ANTHROPIC_API_KEY, GIT_TOKEN, $HOME are all inherited.
	Env []string
}

// NewRunner constructs a Runner. Both fields are optional.
func NewRunner(binary string, extraArgs ...string) *Runner {
	if binary == "" {
		binary = "claude"
	}
	return &Runner{Binary: binary, ExtraArgs: extraArgs}
}

// Verify Runner satisfies runner.Runner at compile time.
var _ runner.Runner = (*Runner)(nil)

func (c *Runner) Name() string { return "claude" }

func (c *Runner) Run(ctx context.Context, req runner.Request) (runner.Response, error) {
	start := time.Now()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := BuildArgs(req, c.ExtraArgs)

	cmd := exec.CommandContext(ctx, c.Binary, args...)
	if req.Worktree != "" {
		cmd.Dir = req.Worktree
	}
	if c.Env != nil {
		cmd.Env = c.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	end := time.Now()
	rawOut := stdout.String()

	// Attempt JSON envelope parse to extract token counts and clean result
	// text. If parsing fails (older CLI version, wrapper script, error page),
	// fall back to treating rawOut as plain text — that preserves backward
	// compatibility and degrades gracefully to Tokens=0.
	resultText, tokens, jsonOK := parseJSONOutput(rawOut)
	if !jsonOK {
		fmt.Fprintf(os.Stderr, "claude: warning: could not parse --output-format json envelope; falling back to plain text (tokens will be 0)\n")
		resultText = rawOut
	}

	if runErr != nil {
		// Even on non-zero exit we try to parse a decision — the model
		// might have flagged failure via the marker before exiting. Fall
		// back to wrapping the OS error.
		decision, summary, question, parseErr := ParseDecision(resultText)
		if parseErr != nil {
			return runner.Response{
					Decision:  runner.DecisionFail,
					Summary:   strings.TrimSpace(stderr.String()),
					Tokens:    tokens,
					StartedAt: start, EndedAt: end,
				}, fmt.Errorf("claude exec: %w (stderr: %s)", runErr,
					strings.TrimSpace(stderr.String()))
		}
		return runner.Response{
			Decision:  decision,
			Summary:   summary,
			Question:  question,
			Tokens:    tokens,
			StartedAt: start, EndedAt: end,
		}, fmt.Errorf("claude exec: %w (parsed decision: %s)", runErr, decision)
	}

	decision, summary, question, err := ParseDecision(resultText)
	if err != nil {
		return runner.Response{Tokens: tokens, StartedAt: start, EndedAt: end},
			fmt.Errorf("parse claude output: %w; raw output:\n%s", err, rawOut)
	}
	return runner.Response{
		Decision:  decision,
		Summary:   summary,
		Question:  question,
		Tokens:    tokens,
		StartedAt: start, EndedAt: end,
	}, nil
}

// --- argv construction ---

// BuildArgs composes the argv passed to the claude binary (everything after
// the binary name). extraArgs is prepended, mirroring Runner.ExtraArgs, so
// callers can still override --model etc. by putting their own flag first —
// claude uses last-flag-wins for repeated flags.
//
// Exported so step bodies / tests can assert what argv a given Request
// produces without shelling out.
func BuildArgs(req runner.Request, extraArgs []string) []string {
	args := append([]string{}, extraArgs...)
	args = append(args,
		"-p", BuildPrompt(req),
		"--output-format", "json", // machine-readable output + token counts
		"--dangerously-skip-permissions", // ADR-0006 — yolo inside the worktree
	)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

// --- prompt construction ---

// BuildPrompt composes the full prompt sent to claude. Exported so step
// bodies can assert what their pre-cooked Goal will get wrapped with.
//
// Layout:
//  1. Header lines (Skill / Unit / Worktree, if set)
//  2. The body — req.Goal, taken verbatim
//  3. Event-specific blocks (comment to address, CI failure to fix)
//  4. The scope-discipline reminder (always appended; flavour depends on
//     whether req.UnitID is set — see ADR-0045)
//  5. The decision-marker protocol instructions (always appended)
func BuildPrompt(req runner.Request) string {
	var b strings.Builder

	if req.SkillCommand != "" {
		fmt.Fprintf(&b, "## Skill\n\n%s\n\n", req.SkillCommand)
	}
	if req.UnitID != "" {
		fmt.Fprintf(&b, "## Unit\n\n%s\n\n", req.UnitID)
	}
	if req.Worktree != "" {
		fmt.Fprintf(&b, "## Worktree\n\n%s\n\n", req.Worktree)
	}

	fmt.Fprintf(&b, "## Task\n\n%s\n\n", req.Goal)

	if req.CommentBody != "" {
		fmt.Fprintf(&b, "## Reviewer feedback to address\n\n%s\n\n", req.CommentBody)
	}
	if req.CIFailure != "" {
		fmt.Fprintf(&b, "## CI failure to investigate\n\n```\n%s\n```\n\n", req.CIFailure)
	}

	// UnitID is only set for unit-execution invocations (see work() in
	// refactorsweep/workflow.go); planning invocations leave it empty.
	if req.UnitID == "" {
		b.WriteString(planningScopeDiscipline)
	} else {
		b.WriteString(unitScopeDiscipline)
	}
	b.WriteString(decisionProtocol)
	return b.String()
}

// planningScopeDiscipline is the scope-discipline flavour for planning
// invocations (req.UnitID == ""). See ADR-0045: a planner left to its own
// devices tends toward a handful of large increments, which is exactly the
// shape that produces multi-concern MRs downstream. This instruction pushes
// the bias the other way, toward more, smaller increments.
const planningScopeDiscipline = `## Scope discipline

Bias toward more, smaller increments rather than fewer, larger ones. Each
increment should cover a single concern that can land as its own small MR.
If a candidate increment would touch more than one concern, split it into
separate increments instead of bundling them together.

`

// unitScopeDiscipline is the scope-discipline flavour for unit-execution
// invocations (req.UnitID != ""). See ADR-0045: units kept ballooning into
// multi-concern MRs because nothing in the prompt told the runner to stay
// narrow — the model reasonably filled the silence by doing as much as it
// could reach.
const unitScopeDiscipline = `## Scope discipline

Keep this unit small and narrowly scoped to the task above. Do only what
is asked — do not bundle in unrelated fixes, refactors, or improvements
you happen to notice along the way. If you spot other work worth doing,
mention it in your summary instead of doing it now; a future unit can
pick it up.

`

// decisionProtocol is the marker-based signalling everflow uses to read
// claude's outcome. Appended to every prompt.
//
// Why prompt-marker over claude's --output-format json: claude's JSON
// surfaces the message stream, not a domain decision. We'd still need a
// way to express "Continue / Done / Ask / Fail / NoChange" — a tail-line
// marker is the smallest contract that does it without depending on
// tool-use registration.
const decisionProtocol = `## How to finish

After completing your work (or deciding you can't), end your response
with EXACTLY ONE of these tags on its own line:

- ` + "`<everflow-decision>continue</everflow-decision>`" + ` — during planning, there's more to do (signals the next increment). During a work turn on a unit, this also means: this unit turned out to be bigger than one turn, you shipped a real partial slice of it, and there's a well-defined remainder left. State the remainder clearly in your summary — what's done and what's left — so the planner can schedule it as a follow-on increment instead of assuming the unit is finished. Don't use it to avoid finishing small units; use it only when the unit genuinely doesn't fit in one turn.
- ` + "`<everflow-decision>done</everflow-decision>`" + ` — task is complete
- ` + "`<everflow-decision>ask: <one-line question></everflow-decision>`" + ` — you need the human's input before proceeding
- ` + "`<everflow-decision>fail: <one-line reason></everflow-decision>`" + ` — you cannot proceed
- ` + "`<everflow-decision>nochange</everflow-decision>`" + ` — nothing to do (e.g. the change was already applied)

The text before the tag becomes the recorded Summary; everflow strips
the tag itself from the output. Only the LAST occurrence of the tag in
your response is read, so feel free to write naturally up to that point.
`

// --- decision parsing ---

// decisionRE matches the closing-paren-style decision marker. We extract
// the LAST one (sometimes the model echoes the protocol back in its
// reasoning before producing the real one).
var decisionRE = regexp.MustCompile(`(?s)<everflow-decision>\s*(.*?)\s*</everflow-decision>`)

// ErrNoDecisionMarker is returned by ParseDecision when claude's response
// contains no <everflow-decision>...</everflow-decision> tag. Treated as a
// runner-level failure by the workflow's step bodies (they see the error
// and pause / fail accordingly).
var ErrNoDecisionMarker = errors.New("claude: no <everflow-decision> marker in response")

// ParseDecision extracts the Decision + Summary + Question from a claude
// response. The Summary is everything before the last marker (trimmed);
// the Question is set only when the decision is "ask".
//
// Exported for tests + for future debugging utilities.
func ParseDecision(out string) (decision runner.Decision, summary, question string, err error) {
	matches := decisionRE.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return runner.DecisionUnknown, strings.TrimSpace(out), "", ErrNoDecisionMarker
	}
	last := matches[len(matches)-1]
	inner := strings.TrimSpace(out[last[2]:last[3]])
	prefix := strings.TrimSpace(out[:last[0]])

	// Inner can be "<verb>" or "<verb>: <text>"
	verb, rest := splitVerb(inner)
	switch verb {
	case "continue":
		return runner.DecisionContinue, prefix, "", nil
	case "done":
		return runner.DecisionDone, prefix, "", nil
	case "ask":
		summary = prefix
		question = rest
		if question == "" {
			question = "(no question text)"
		}
		return runner.DecisionAsk, summary, question, nil
	case "fail":
		summary = prefix
		if rest != "" {
			// Surface the reason in Summary too so it ends up in the
			// MR comment + audit even if Question wasn't set.
			summary = strings.TrimSpace(summary + "\n\nReason: " + rest)
		}
		return runner.DecisionFail, summary, "", nil
	case "nochange":
		return runner.DecisionNoChange, prefix, "", nil
	default:
		return runner.DecisionUnknown, prefix, "",
			fmt.Errorf("claude: unrecognised decision verb %q", verb)
	}
}

// splitVerb breaks "<verb>" or "<verb>: <rest>" into (verb, rest). Verb
// is lowercased and trimmed; rest is everything after the first colon
// (also trimmed). Both can be empty.
func splitVerb(inner string) (verb, rest string) {
	idx := strings.IndexByte(inner, ':')
	if idx == -1 {
		return strings.ToLower(strings.TrimSpace(inner)), ""
	}
	return strings.ToLower(strings.TrimSpace(inner[:idx])),
		strings.TrimSpace(inner[idx+1:])
}
