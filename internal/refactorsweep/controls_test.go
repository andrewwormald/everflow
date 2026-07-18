package refactorsweep

import (
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
)

// --- parser ---

func TestParseControlVerb(t *testing.T) {
	cases := []struct {
		in       string
		wantVerb string
		wantArgs string
	}{
		{"/everflow pause", "pause", ""},
		{"  /everflow pause  ", "pause", ""},
		{"/everflow PAUSE", "pause", ""},
		{"/everflow skip ran out of time", "skip", "ran out of time"},
		{"/everflow prompt focus on auth first", "prompt", "focus on auth first"},
		{"/everflow prompt\nuse log/slog\nnot logrus", "prompt", "use log/slog\nnot logrus"},
		{"/everflow", "", ""},
		{"/everflow   ", "", ""},
		{"not a command", "", ""},
		{"hello /everflow pause", "", ""}, // must be at start
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			v, a := parseControlVerb(tc.in)
			if v != tc.wantVerb || a != tc.wantArgs {
				t.Errorf("parseControlVerb(%q) = (%q, %q); want (%q, %q)",
					tc.in, v, a, tc.wantVerb, tc.wantArgs)
			}
		})
	}
}

// --- helpers ---

// controlEvent builds a /everflow comment event from the author, on the
// MR the awaitingRun helper already has in flight.
func controlEvent(body string, mr provider.MR) provider.Event {
	return provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"}, // matches awaitingRun's Author
		Note:   provider.Note{Body: body},
	}
}

// --- individual verb tests ---

// TestResume_ControlCommand_ReactsBeforeHandling asserts the generic
// /everflow dispatch path (workflow.go's second handleControlCommand call
// site) acknowledges the triggering comment with a reaction, same as the
// subagent-invocation path does.
func TestResume_ControlCommand_ReactsBeforeHandling(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"},
		Note:   provider.Note{ID: 7, Stream: "issue_comment", Body: "/everflow pause taking lunch"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("want Paused, got %v", next)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("want exactly 1 reaction; got %+v", fp.reactions)
	}
	got := fp.reactions[0]
	want := reactToNoteCall{ProjectID: "x/y", MRIID: 1, NoteID: 7, Stream: "issue_comment", Emoji: "eyes"}
	if got != want {
		t.Errorf("ReactToNote call = %+v, want %+v", got, want)
	}
}

func TestCmdPause(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow pause taking lunch", mr)))
	if next != StatusPaused {
		t.Errorf("want Paused, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "taking lunch") {
		t.Errorf("PauseReason should include args: %q", r.Object.PauseReason)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "Paused") {
		t.Errorf("expected ack comment; got %+v", fp.comments)
	}
}

func TestCmdResume(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "was paused for a reason"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow resume", mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared; got %q", r.Object.PauseReason)
	}
}

func TestCmdSkip(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 42}
	r := awaitingRun(t, "svc-a", mr)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow skip too risky", mr)))
	if next != StatusDiscovering {
		t.Errorf("want Discovering after skip, got %v", next)
	}
	if len(fp.closes) != 1 || fp.closes[0].IID != 42 {
		t.Errorf("CloseMR should have been called for MR 42; got %+v", fp.closes)
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "svc-a" {
		t.Errorf("svc-a should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "too risky") {
		t.Errorf("blacklist reason should include args: %q", r.Object.Blacklisted[0].Reason)
	}
}

func TestCmdSkip_UnknownMR(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	otherMR := provider.MR{ProjectID: "x/y", IID: 99}
	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow skip", otherMR)))
	if next == StatusDiscovering {
		t.Errorf("skip on untracked MR should not transition; got %v", next)
	}
	if len(fp.closes) != 0 {
		t.Errorf("untracked MR should not be closed; got %+v", fp.closes)
	}
}

func TestCmdRetry(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "push failed"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow retry", mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after retry, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("retry should clear PauseReason; got %q", r.Object.PauseReason)
	}
}

func TestCmdPrompt_StoresInjection(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	body := "/everflow prompt focus on tests first, then the lint errors"
	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent(body, mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("prompt should not change state when in AwaitingMerge; got %v", next)
	}
	if !strings.Contains(r.Object.PromptInjection, "focus on tests first") {
		t.Errorf("PromptInjection not stored: %q", r.Object.PromptInjection)
	}
}

func TestCmdPrompt_NoArgs(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow prompt", mr)))
	if r.Object.PromptInjection != "" {
		t.Errorf("bare /everflow prompt should not set injection; got %q", r.Object.PromptInjection)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "needs text") {
		t.Errorf("expected error comment; got %+v", fp.comments)
	}
}

func TestPromptInjection_ConsumedByNextRunnerCall(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.PromptInjection = "remember to handle the nil case"

	// A normal comment (not a control verb) triggers invokeForEvent. The
	// runner's Goal should now carry the injected prompt; PromptInjection
	// should be cleared after.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename"},
	}
	d.resume(t.Context(), r, payloadOf(t, ev))

	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].Goal, "nil case") {
		t.Errorf("Goal should carry the injected prompt: %q", fr.calls[0].Goal)
	}
	if r.Object.PromptInjection != "" {
		t.Errorf("PromptInjection should be cleared after use; got %q", r.Object.PromptInjection)
	}
}

func TestCmdStatus_PostsSummary(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.Goal = "Migrate to slog"
	r.Object.Completed = []CompletedUnit{{UnitID: "a"}, {UnitID: "b"}}
	r.Object.SubagentInvocations = 12

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow status", mr)))
	if len(fp.comments) != 1 {
		t.Fatalf("expected one status comment; got %+v", fp.comments)
	}
	body := fp.comments[0].Body
	for _, want := range []string{"Migrate to slog", "2 completed", "Subagent invocations: 12"} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %q; got:\n%s", want, body)
		}
	}
}

func TestCmdStop(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "u", mr)
	r.Object.BaseRepo = "/tmp/fake"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow stop done with this", mr)))
	if next != StatusCancelled {
		t.Errorf("want Cancelled, got %v", next)
	}
	if len(fp.closes) != 1 || fp.closes[0].IID != 7 {
		t.Errorf("in-flight MRs should be closed on stop; got %+v", fp.closes)
	}
	if !strings.Contains(r.Object.LastError, "/everflow stop") {
		t.Errorf("LastError should record cancellation: %q", r.Object.LastError)
	}
	// Verify the final comment got posted (before the close to maximise
	// visibility).
	foundStop := false
	for _, c := range fp.comments {
		if strings.Contains(c.Body, "Stopped") {
			foundStop = true
			break
		}
	}
	if !foundStop {
		t.Errorf("expected a 'Stopped' final comment; got %+v", fp.comments)
	}
}

func TestCmdHelp_BareEverflow(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow", mr)))
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "control verbs") {
		t.Errorf("bare /everflow should post help; got %+v", fp.comments)
	}
}

func TestCmdFreeform_InvokesSubagent(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "refactored the auth module",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	next, err := d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow refactor the auth module first", mr)))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be invoked once for a freeform verb; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].Goal, "refactor the auth module first") {
		t.Errorf("freeform instruction not injected into Goal: %q", fr.calls[0].Goal)
	}
	if r.Object.PromptInjection != "" {
		t.Errorf("PromptInjection should be consumed (single-use); got %q", r.Object.PromptInjection)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "refactored the auth module") {
		t.Errorf("status comment should be posted on DecisionDone; got %+v", fp.comments)
	}
}

func TestCmdFreeform_UntrackedMR_RepliesWithHelp(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 999} // not tracked by any in-flight unit
	r := awaitingRun(t, "u", provider.MR{ProjectID: "x/y", IID: 1})

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/everflow foobar", mr)))
	if len(fr.calls) != 0 {
		t.Errorf("runner should not be invoked for an untracked MR; got %d calls", len(fr.calls))
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "isn't tracked") {
		t.Errorf("untracked MR should get a polite reply; got %+v", fp.comments)
	}
}

func TestNonAuthor_ControlComment_FallsThrough(t *testing.T) {
	// A reviewer typing /everflow pause should NOT pause the Run — they
	// have no control privileges (ADR-0017). The /everflow detection only
	// triggers for the Run author.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionNoChange, Summary: "noted"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "imposter"}, // NOT the author
		Note:   provider.Note{Body: "/everflow pause"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next == StatusPaused {
		t.Errorf("non-author /everflow should not pause; got %v", next)
	}
}
