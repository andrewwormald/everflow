package claude

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrewwormald/everflow/internal/runner"
)

func TestParseDecision_Done(t *testing.T) {
	out := `Sure, I'll do this.

[work happens]

I've updated the file.

<everflow-decision>done</everflow-decision>`
	d, summary, q, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done, got %v", d)
	}
	if !strings.Contains(summary, "updated the file") {
		t.Errorf("Summary should carry the body: %q", summary)
	}
	if q != "" {
		t.Errorf("Question should be empty for Done; got %q", q)
	}
}

func TestParseDecision_Continue(t *testing.T) {
	out := `Planning next: migrate services/payments to slog.

<everflow-decision>continue</everflow-decision>`
	d, summary, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionContinue {
		t.Errorf("Decision: want Continue, got %v", d)
	}
	if !strings.Contains(summary, "migrate services/payments") {
		t.Errorf("Summary should carry rationale: %q", summary)
	}
}

func TestParseDecision_AskWithQuestion(t *testing.T) {
	out := `I'm not sure about the deprecated middleware.

<everflow-decision>ask: Should I migrate the deprecated middleware too, or skip it?</everflow-decision>`
	d, _, q, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionAsk {
		t.Errorf("Decision: want Ask, got %v", d)
	}
	if !strings.Contains(q, "deprecated middleware too") {
		t.Errorf("Question not extracted: %q", q)
	}
}

func TestParseDecision_AskWithoutQuestion(t *testing.T) {
	// Defensive: if the model forgets the question, we default to
	// "(no question text)" rather than panicking.
	out := `<everflow-decision>ask:</everflow-decision>`
	d, _, q, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionAsk {
		t.Errorf("Decision: want Ask, got %v", d)
	}
	if q == "" {
		t.Errorf("Question should default to placeholder, not empty")
	}
}

func TestParseDecision_FailWithReason(t *testing.T) {
	out := `Tried three times.

<everflow-decision>fail: Could not resolve the API contract drift</everflow-decision>`
	d, summary, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionFail {
		t.Errorf("Decision: want Fail, got %v", d)
	}
	if !strings.Contains(summary, "API contract drift") {
		t.Errorf("Summary should include the reason: %q", summary)
	}
}

func TestParseDecision_NoChange(t *testing.T) {
	out := `Looked at the codebase; this change is already applied.

<everflow-decision>nochange</everflow-decision>`
	d, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionNoChange {
		t.Errorf("Decision: want NoChange, got %v", d)
	}
}

func TestParseDecision_NoMarker(t *testing.T) {
	out := `I wrote some text but forgot the marker.`
	_, _, _, err := ParseDecision(out)
	if !errors.Is(err, ErrNoDecisionMarker) {
		t.Fatalf("want ErrNoDecisionMarker, got %v", err)
	}
}

func TestParseDecision_UnknownVerb(t *testing.T) {
	out := `<everflow-decision>bonkers</everflow-decision>`
	_, _, _, err := ParseDecision(out)
	if err == nil {
		t.Fatalf("want error for unknown verb")
	}
	if !strings.Contains(err.Error(), "unrecognised") {
		t.Errorf("error should mention unrecognised verb: %v", err)
	}
}

func TestParseDecision_LastMarkerWins(t *testing.T) {
	// The model sometimes echoes the protocol in its reasoning before
	// producing the real decision. We must pick the LAST occurrence.
	out := `Let me think. I could finish with <everflow-decision>continue</everflow-decision>
but actually I'm done now.

<everflow-decision>done</everflow-decision>`
	d, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done (last marker), got %v", d)
	}
}

func TestParseDecision_CaseInsensitiveVerb(t *testing.T) {
	out := `<everflow-decision>DONE</everflow-decision>`
	d, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done, got %v", d)
	}
}

func TestBuildPrompt_AllFields(t *testing.T) {
	req := runner.Request{
		SkillCommand: "/everflow-address-comment svc-payments",
		UnitID:       "svc-payments",
		Worktree:     "/home/everflow/run-xyz/worktrees/svc-payments",
		Goal:         "Migrate logrus to log/slog. Preserve log levels.",
		CommentBody:  "please also rename the LogContext type",
		CIFailure:    "FAIL: TestSomething (panic: nil pointer)",
	}
	prompt := BuildPrompt(req)

	for _, want := range []string{
		"## Skill", "/everflow-address-comment svc-payments",
		"## Unit", "svc-payments",
		"## Worktree", "/home/everflow/run-xyz",
		"## Task", "Migrate logrus",
		"## Reviewer feedback to address", "rename the LogContext",
		"## CI failure to investigate", "TestSomething",
		"## Scope discipline", "small and narrowly scoped",
		"## How to finish", "<everflow-decision>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

func TestBuildPrompt_MinimalFields(t *testing.T) {
	// Only Goal set, no UnitID — this is the planning shape. No headers
	// should be rendered for empty fields, but the decision protocol and
	// the planning flavour of scope discipline are always present.
	req := runner.Request{Goal: "just do the thing"}
	prompt := BuildPrompt(req)
	if strings.Contains(prompt, "## Skill") {
		t.Errorf("empty Skill should not produce a Skill header")
	}
	if strings.Contains(prompt, "## Reviewer feedback") {
		t.Errorf("empty CommentBody should not produce a Reviewer header")
	}
	if !strings.Contains(prompt, "just do the thing") {
		t.Errorf("Goal should be present")
	}
	if !strings.Contains(prompt, "<everflow-decision>") {
		t.Errorf("decision protocol should always be appended")
	}
	if !strings.Contains(prompt, "## Scope discipline") {
		t.Errorf("scope discipline reminder should always be appended, even with minimal fields")
	}
	if !strings.Contains(prompt, "more, smaller increments") {
		t.Errorf("no UnitID should get the planning scope-discipline flavour")
	}
	if strings.Contains(prompt, "small and narrowly scoped") {
		t.Errorf("no UnitID should not get the unit-scoped flavour")
	}
}

func TestBuildPrompt_ScopeDiscipline_PlanningVsUnit(t *testing.T) {
	// req.UnitID is the discriminator between the two scope-discipline
	// flavours: empty means a planning invocation, set means a unit
	// invocation. See ADR-0045.
	planning := BuildPrompt(runner.Request{Goal: "plan the next increment"})
	if !strings.Contains(planning, "more, smaller increments") {
		t.Errorf("planning invocation (no UnitID) should get the planning flavour")
	}
	if strings.Contains(planning, "small and narrowly scoped") {
		t.Errorf("planning invocation should not get the unit flavour")
	}

	unit := BuildPrompt(runner.Request{Goal: "do the unit", UnitID: "svc-payments"})
	if !strings.Contains(unit, "small and narrowly scoped") {
		t.Errorf("unit invocation (UnitID set) should get the unit flavour")
	}
	if strings.Contains(unit, "more, smaller increments") {
		t.Errorf("unit invocation should not get the planning flavour")
	}
}

func TestBuildPrompt_ScopeDisciplinePrecedesDecisionProtocol(t *testing.T) {
	// The scope reminder must land before the decision-marker instructions,
	// so it reads as guidance on the work rather than on how to answer.
	req := runner.Request{Goal: "just do the thing"}
	prompt := BuildPrompt(req)

	scopeIdx := strings.Index(prompt, "## Scope discipline")
	decisionIdx := strings.Index(prompt, "## How to finish")
	if scopeIdx == -1 || decisionIdx == -1 {
		t.Fatalf("expected both sections present: scopeIdx=%d decisionIdx=%d", scopeIdx, decisionIdx)
	}
	if scopeIdx >= decisionIdx {
		t.Errorf("scope discipline should precede the decision protocol; scopeIdx=%d decisionIdx=%d", scopeIdx, decisionIdx)
	}
}

// --- BuildArgs tests ---

func TestBuildArgs_NoModel(t *testing.T) {
	req := runner.Request{Goal: "do the thing"}
	args := BuildArgs(req, nil)
	for _, a := range args {
		if a == "--model" {
			t.Fatalf("no --model flag expected when Model is unset; got %v", args)
		}
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	req := runner.Request{Goal: "do the thing", Model: "claude-haiku-4-5"}
	args := BuildArgs(req, nil)

	idx := -1
	for i, a := range args {
		if a == "--model" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("--model flag not found in args: %v", args)
	}
	if idx+1 >= len(args) || args[idx+1] != "claude-haiku-4-5" {
		t.Errorf("--model value: got %v", args)
	}
}

func TestBuildArgs_ExtraArgsPrepended(t *testing.T) {
	req := runner.Request{Goal: "do the thing"}
	args := BuildArgs(req, []string{"--debug"})
	if args[0] != "--debug" {
		t.Errorf("extraArgs should be prepended; got %v", args)
	}
}

func TestRunner_Name(t *testing.T) {
	if got := (&Runner{}).Name(); got != "claude" {
		t.Errorf("Name: want claude, got %q", got)
	}
	if got := NewRunner("/usr/local/bin/claude").Name(); got != "claude" {
		t.Errorf("Name: want claude regardless of binary, got %q", got)
	}
}

// --- parseJSONOutput tests ---

func TestParseJSONOutput_WithUsage(t *testing.T) {
	// Simulates `claude -p --output-format json` output when the CLI populates
	// the usage block with real token counts.
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"I updated the file.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_abc","cost_usd":0.01,"total_cost_usd":0.01,"duration_ms":3000,"num_turns":1,"usage":{"input_tokens":800,"cache_creation_input_tokens":0,"cache_read_input_tokens":200,"output_tokens":120}}`
	result, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput returned ok=false; want ok=true")
	}
	if !strings.Contains(result, "<everflow-decision>done</everflow-decision>") {
		t.Errorf("result text should contain decision marker, got: %q", result)
	}
	// 800 + 0 + 200 + 120 = 1120
	if tokens != 1120 {
		t.Errorf("tokens: want 1120, got %d", tokens)
	}
}

func TestParseJSONOutput_NoUsage(t *testing.T) {
	// Older claude CLI build that omits the usage field should still parse
	// correctly, returning tokens=0 rather than erroring.
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"Done.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_xyz","cost_usd":0.005,"duration_ms":1500,"num_turns":1}`
	result, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput returned ok=false; want ok=true")
	}
	if !strings.Contains(result, "Done") {
		t.Errorf("result text should include response body, got: %q", result)
	}
	if tokens != 0 {
		t.Errorf("tokens with missing usage block: want 0, got %d", tokens)
	}
}

func TestParseJSONOutput_InvalidJSON(t *testing.T) {
	// Plain-text output (e.g. from a wrapper script that doesn't use JSON mode).
	raw := "I updated the file.\n\n<everflow-decision>done</everflow-decision>"
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with plain text: want ok=false")
	}
}

func TestParseJSONOutput_WrongType(t *testing.T) {
	// JSON but not a result envelope (e.g. an error object from the platform).
	raw := `{"type":"error","message":"API rate limit exceeded"}`
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with non-result type: want ok=false")
	}
}

func TestParseJSONOutput_EmptyResult(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":""}`
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with empty result: want ok=false (no marker possible)")
	}
}

// TestParseDecision_FullRoundTrip checks that a real claude JSON output
// round-trips through parseJSONOutput → ParseDecision end-to-end.
func TestParseDecision_FullRoundTrip(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"I migrated the logrus calls to slog.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_rt","cost_usd":0.02,"duration_ms":4000,"num_turns":2,"usage":{"input_tokens":500,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":80}}`
	resultText, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput: want ok=true")
	}
	d, summary, _, err := ParseDecision(resultText)
	if err != nil {
		t.Fatalf("ParseDecision: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("decision: want Done, got %v", d)
	}
	if !strings.Contains(summary, "migrated the logrus calls") {
		t.Errorf("summary should contain response text, got: %q", summary)
	}
	// 500 + 100 + 0 + 80 = 680
	if tokens != 680 {
		t.Errorf("tokens: want 680, got %d", tokens)
	}
}
