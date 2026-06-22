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
		"## How to finish", "<everflow-decision>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

func TestBuildPrompt_MinimalFields(t *testing.T) {
	// Only Goal set — no headers should be rendered for empty fields,
	// but the decision protocol is always present.
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
}

func TestRunner_Name(t *testing.T) {
	if got := (&Runner{}).Name(); got != "claude" {
		t.Errorf("Name: want claude, got %q", got)
	}
	if got := NewRunner("/usr/local/bin/claude").Name(); got != "claude" {
		t.Errorf("Name: want claude regardless of binary, got %q", got)
	}
}
