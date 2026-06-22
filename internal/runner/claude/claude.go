// Package claude implements runner.Runner by shelling out to the
// `claude` CLI. See ADR-0027 for the prompt-marker protocol and
// ADR-0004 for the original "shell out, not the SDK" decision.
//
// The adapter is dumb: it composes a prompt from the runner.Request
// fields (Goal, Worktree, UnitID, CommentBody, CIFailure), appends a
// decision-marker instruction, runs `claude -p`, and parses the marker
// out of the response. It does not interpret SkillCommand — the step
// body is responsible for setting Goal to a fully-formed task; this
// adapter just adds the protocol envelope claude needs to signal back.
package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/andrewwormald/everflow/internal/runner"
)

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

	prompt := BuildPrompt(req)

	args := append([]string{}, c.ExtraArgs...)
	args = append(args,
		"-p", prompt,
		"--dangerously-skip-permissions", // ADR-0006 — yolo inside the worktree
	)

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

	if runErr != nil {
		// Even on non-zero exit we try to parse a decision — the model
		// might have flagged failure via the marker before exiting. Fall
		// back to wrapping the OS error.
		decision, summary, question, parseErr := ParseDecision(rawOut)
		if parseErr != nil {
			return runner.Response{
				Decision:  runner.DecisionFail,
				Summary:   strings.TrimSpace(stderr.String()),
				StartedAt: start, EndedAt: end,
			}, fmt.Errorf("claude exec: %w (stderr: %s)", runErr,
				strings.TrimSpace(stderr.String()))
		}
		return runner.Response{
			Decision:  decision,
			Summary:   summary,
			Question:  question,
			StartedAt: start, EndedAt: end,
		}, fmt.Errorf("claude exec: %w (parsed decision: %s)", runErr, decision)
	}

	decision, summary, question, err := ParseDecision(rawOut)
	if err != nil {
		return runner.Response{StartedAt: start, EndedAt: end},
			fmt.Errorf("parse claude output: %w; raw output:\n%s", err, rawOut)
	}
	return runner.Response{
		Decision:  decision,
		Summary:   summary,
		Question:  question,
		StartedAt: start, EndedAt: end,
		// Tokens stays 0 — we don't yet parse claude's JSON output mode
		// to extract token counts. Future ADR.
	}, nil
}

// --- prompt construction ---

// BuildPrompt composes the full prompt sent to claude. Exported so step
// bodies can assert what their pre-cooked Goal will get wrapped with.
//
// Layout:
//   1. Header lines (Skill / Unit / Worktree, if set)
//   2. The body — req.Goal, taken verbatim
//   3. Event-specific blocks (comment to address, CI failure to fix)
//   4. The decision-marker protocol instructions (always appended)
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

	b.WriteString(decisionProtocol)
	return b.String()
}

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

- ` + "`<everflow-decision>continue</everflow-decision>`" + ` — there's more to do (used during planning to signal the next increment)
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
