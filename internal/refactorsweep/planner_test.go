package refactorsweep

import (
	"errors"
	"strings"
	"testing"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
)

// specRunInDiscover returns a Run already set up in spec mode, in
// StatusDiscovering (the state where discover() fires). Use this fixture
// for planner tests.
func specRunInDiscover(t *testing.T, plan []PlannedIncrement) *workflow.Run[AgentState, AgentStatus] {
	t.Helper()
	r := newRun(t, &AgentState{
		Mode:         ModeSpec,
		Goal:         "Migrate to slog",
		SpecBody:     "Replace logrus calls with log/slog across all services.",
		ProviderName: "fake",
		ProjectID:    "x/y",
		RunnerName:   "fake-runner",
		Plan:         plan,
		InFlight:     map[string]provider.MR{},
	})
	r.Status = StatusDiscovering
	return r
}

func TestDiscover_SpecMode_PlannerContinuesWithNewIncrement(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionContinue,
		Summary:  "Migrate services/payments to slog",
		Tokens:   500,
	}})
	r := specRunInDiscover(t, nil)

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusWorking {
		t.Errorf("want StatusWorking, got %v", next)
	}
	if r.Object.CurrentUnit != "increment-1" {
		t.Errorf("CurrentUnit: want increment-1, got %q", r.Object.CurrentUnit)
	}
	if len(r.Object.Plan) != 1 || r.Object.Plan[0].UnitID != "increment-1" {
		t.Errorf("Plan: want one increment-1 entry; got %+v", r.Object.Plan)
	}
	if r.Object.Plan[0].Rationale != "Migrate services/payments to slog" {
		t.Errorf("Rationale not captured: %q", r.Object.Plan[0].Rationale)
	}
	if r.Object.Plan[0].Outcome != "in_flight" {
		t.Errorf("Outcome: want in_flight, got %q", r.Object.Plan[0].Outcome)
	}

	// Planning Turn should be recorded with phase="plan".
	if len(r.Object.History) != 1 || r.Object.History[0].Phase != "plan" {
		t.Errorf("expected one history turn with phase=plan; got %+v", r.Object.History)
	}
	// Runner was called with no UnitID (planning is not unit-scoped).
	if len(fr.calls) != 1 || fr.calls[0].UnitID != "" {
		t.Errorf("runner.UnitID should be empty for planning; got %+v", fr.calls)
	}
	if !strings.Contains(fr.calls[0].Goal, "Migrate to slog") {
		t.Errorf("Goal should carry the spec context; got %q", fr.calls[0].Goal)
	}
}

func TestDiscover_SpecMode_PlannerDone_CompletesRun(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Spec is fully implemented; no further increments needed.",
	}})
	r := specRunInDiscover(t, []PlannedIncrement{
		{UnitID: "increment-1", Outcome: "completed", Rationale: "first slice"},
	})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusCompleted {
		t.Errorf("want StatusCompleted on planner=Done, got %v", next)
	}
}

func TestDiscover_SpecMode_PlannerNoChange_Completes(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionNoChange,
		Summary:  "Nothing actionable to plan right now",
	}})
	r := specRunInDiscover(t, nil)

	next, _ := d.discover(t.Context(), r)
	if next != StatusCompleted {
		t.Errorf("want Completed on NoChange (nothing to do), got %v", next)
	}
}

func TestDiscover_SpecMode_PlannerAsks_Pauses(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionAsk,
		Question: "Should I refactor the deprecated middleware too?",
	}})
	r := specRunInDiscover(t, nil)

	next, _ := d.discover(t.Context(), r)
	if next != StatusPaused {
		t.Errorf("want StatusPaused on Ask, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "deprecated middleware") {
		t.Errorf("PauseReason should carry the planner's question: %q", r.Object.PauseReason)
	}
}

func TestDiscover_SpecMode_PlannerFails(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionFail,
		Summary:  "I can't make sense of this spec",
	}})
	r := specRunInDiscover(t, nil)

	next, _ := d.discover(t.Context(), r)
	if next != StatusFailed {
		t.Errorf("want StatusFailed on planner Fail, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "can't make sense") {
		t.Errorf("LastError should carry planner's reason: %q", r.Object.LastError)
	}
}

func TestDiscover_SpecMode_RunnerError(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{err: errors.New("rate limited")})
	r := specRunInDiscover(t, nil)

	next, err := d.discover(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from runner failure")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
}

func TestDiscover_SpecMode_BuildsPromptFromHistory(t *testing.T) {
	// The planner gets the spec body + plan history in the Goal. Verify
	// that previous increments and their outcomes are surfaced — the
	// planner needs this to avoid repeating itself.
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	r := specRunInDiscover(t, []PlannedIncrement{
		{UnitID: "increment-1", Rationale: "migrate payments", Outcome: "completed"},
		{UnitID: "increment-2", Rationale: "migrate kyc", Outcome: "blacklisted"},
	})

	d.discover(t.Context(), r)

	prompt := fr.calls[0].Goal
	for _, want := range []string{
		"Migrate to slog",
		"increment-1",
		"completed",
		"migrate payments",
		"increment-2",
		"blacklisted",
		"migrate kyc",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planning prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

func TestDiscover_SpecMode_PromptInjectionConsumedOnPlanning(t *testing.T) {
	// /syntropy prompt should be applied to the next planning call too,
	// not just to work() / invokeForEvent.
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	r := specRunInDiscover(t, nil)
	r.Object.PromptInjection = "remember to chunk by service boundary"

	d.discover(t.Context(), r)

	if !strings.Contains(fr.calls[0].Goal, "chunk by service boundary") {
		t.Errorf("injection should be in planning Goal: %q", fr.calls[0].Goal)
	}
	if r.Object.PromptInjection != "" {
		t.Errorf("injection should be consumed; got %q", r.Object.PromptInjection)
	}
}

func TestDiscover_SpecMode_UnitIDsAreSequential(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionContinue,
		Summary:  "Next slice",
	}})
	// Plan already has 3 entries; the new increment should be increment-4.
	r := specRunInDiscover(t, []PlannedIncrement{
		{UnitID: "increment-1", Outcome: "completed"},
		{UnitID: "increment-2", Outcome: "completed"},
		{UnitID: "increment-3", Outcome: "blacklisted"},
	})

	d.discover(t.Context(), r)
	if r.Object.CurrentUnit != "increment-4" {
		t.Errorf("CurrentUnit: want increment-4, got %q", r.Object.CurrentUnit)
	}
}

func TestDiscover_SweepMode_UnchangedByModeField(t *testing.T) {
	// Explicit ModeSweep should behave identically to empty Mode (the
	// "before" tests cover empty Mode).
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	r := newRun(t, &AgentState{
		Mode:     ModeSweep,
		Queue:    []string{"svc-a", "svc-b"},
		InFlight: map[string]provider.MR{},
	})

	next, _ := d.discover(t.Context(), r)
	if next != StatusWorking {
		t.Errorf("want Working, got %v", next)
	}
	if r.Object.CurrentUnit != "svc-a" {
		t.Errorf("CurrentUnit: want svc-a, got %q", r.Object.CurrentUnit)
	}
	// Plan should not be touched in sweep mode.
	if len(r.Object.Plan) != 0 {
		t.Errorf("Plan should stay empty in sweep mode; got %+v", r.Object.Plan)
	}
}

func TestMarkUnitMerged_UpdatesPlanOutcome(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	r := newRun(t, &AgentState{
		Mode:     ModeSpec,
		InFlight: map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "increment-1", Outcome: "in_flight", Rationale: "first slice"},
		},
	})

	d.markUnitMerged(t.Context(), r, "increment-1", provider.MR{})
	if r.Object.Plan[0].Outcome != "completed" {
		t.Errorf("Plan outcome should flip to completed; got %q", r.Object.Plan[0].Outcome)
	}
}

func TestDiscover_SpecMode_SetsUpPlanningWorktree(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	g := d.withGit(&fakeGit{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionContinue, Summary: "next slice"}})

	r := specRunInDiscover(t, nil)
	r.Object.BaseRepo = "/some/repo"
	r.Object.BaseBranch = "main"

	if _, err := d.discover(t.Context(), r); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(g.ensures) != 1 {
		t.Fatalf("EnsureBranch should be called once for the planning worktree; got %d", len(g.ensures))
	}
	ec := g.ensures[0]
	if !strings.Contains(ec.Dir, "planning") {
		t.Errorf("planning dir should be under 'planning'; got %q", ec.Dir)
	}
	if !strings.HasPrefix(ec.Branch, "everflow/plan/") {
		t.Errorf("plan branch should be everflow/plan/<id>; got %q", ec.Branch)
	}
	if ec.BaseBranch != "main" {
		t.Errorf("BaseBranch: want main, got %q", ec.BaseBranch)
	}
	if len(g.resets) != 1 {
		t.Errorf("HardReset should be called once to refresh planning worktree; got %d", len(g.resets))
	}
}

func TestDiscover_SpecMode_SkipsWorktreeWhenBaseRepoEmpty(t *testing.T) {
	// Tests that don't care about git (most existing planner tests) pass
	// BaseRepo="". The worktree setup should be skipped in that case so
	// fakes don't have to handle it.
	d := newDeps(t, &fakeProvider{})
	g := d.withGit(&fakeGit{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	r := specRunInDiscover(t, nil)
	r.Object.BaseRepo = ""

	if _, err := d.discover(t.Context(), r); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(g.ensures) != 0 || len(g.resets) != 0 {
		t.Errorf("worktree setup should be skipped when BaseRepo=''")
	}
}

func TestMarkUnitBlacklisted_UpdatesPlanOutcome(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	r := newRun(t, &AgentState{
		Mode:     ModeSpec,
		InFlight: map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "increment-1", Outcome: "in_flight"},
		},
	})

	d.markUnitBlacklisted(t.Context(), r, "increment-1", provider.MR{}, "rejected")
	if r.Object.Plan[0].Outcome != "blacklisted" {
		t.Errorf("Plan outcome should flip to blacklisted; got %q", r.Object.Plan[0].Outcome)
	}
}
