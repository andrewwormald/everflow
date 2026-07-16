package refactorsweep

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/everflow/internal/provider"
)

// Control-verb dispatcher for /everflow comments from the Run's author.
// See ADR-0017 for the privilege model and the verb set.
//
// Detected in resume() via the "/everflow" prefix on author-comments; the
// filter never runs on these. handleControlCommand parses verb + args and
// fans out to per-verb handlers. Each handler is responsible for posting
// an acknowledgement comment so the MR thread shows what the workflow did.

// parseControlVerb extracts (verb, args) from a comment body. The verb is
// the first whitespace-separated token after "/everflow", lowercased. args
// is everything after the verb, with surrounding whitespace trimmed.
// Multi-line args are preserved.
//
// Examples:
//
//	"/everflow pause"               → ("pause", "")
//	"/everflow skip ran out of time" → ("skip", "ran out of time")
//	"/everflow prompt\nuse log/slog\nnot logrus" → ("prompt", "use log/slog\nnot logrus")
//	"/everflow"                     → ("", "")   ← bare invocation; help
//	"not a command"                 → ("", "")   ← caller is expected to gate
func parseControlVerb(body string) (verb, args string) {
	s := strings.TrimSpace(body)
	if !strings.HasPrefix(s, "/everflow") {
		return "", ""
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "/everflow"))
	if s == "" {
		return "", ""
	}
	i := strings.IndexAny(s, " \t\n")
	if i == -1 {
		return strings.ToLower(s), ""
	}
	return strings.ToLower(s[:i]), strings.TrimSpace(s[i:])
}

func (d *Deps) handleControlCommand(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) (AgentStatus, error) {
	verb, args := parseControlVerb(ev.Note.Body)
	switch verb {
	case "pause":
		return d.cmdPause(ctx, r, ev, args)
	case "resume":
		return d.cmdResume(ctx, r, ev, args)
	case "skip":
		return d.cmdSkip(ctx, r, ev, args)
	case "retry":
		return d.cmdRetry(ctx, r, ev, args)
	case "prompt":
		return d.cmdPrompt(ctx, r, ev, args)
	case "status":
		return d.cmdStatus(ctx, r, ev, args)
	case "stop":
		return d.cmdStop(ctx, r, ev, args)
	case "abandon":
		return d.cmdAbandon(ctx, r, ev, args)
	case "":
		return d.cmdHelp(ctx, r, ev)
	default:
		return d.cmdFreeform(ctx, r, ev, verb)
	}
}

// cmdAbandon is the two-tap "are you sure?" stop. First /everflow abandon
// transitions to StatusAwaitingAbandonConfirm with a confirmation prompt;
// a second /everflow abandon within 12h confirms and transitions to
// StatusCancelled (closing in-flight MRs along the way). Anything else
// during the 12h window cancels the abandon — see resume()'s
// AwaitingAbandonConfirm branch and dropAbandonConfirm.
//
// Difference vs /everflow stop: /stop is one-tap, no confirmation. Use
// /stop when you're sure; /abandon when you want a moment to reconsider.
// See ADR-0026.
func (d *Deps) cmdAbandon(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]

	if r.Status == StatusAwaitingAbandonConfirm {
		// Confirmation tap. Mirror /everflow stop's terminal flow.
		body := fmt.Sprintf("🛑 Confirmed abandonment by @%s. Closing in-flight MRs; run cancelled.", ev.Author.Handle)
		if args != "" {
			body = fmt.Sprintf("%s\n\nReason: %s", body, args)
		}
		_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, body)
		for unitID, mr := range r.Object.InFlight {
			_ = p.CloseMR(ctx, mr.ProjectID, mr.IID)
			d.cleanupWorktree(ctx, r, unitID)
		}
		r.Object.LastError = fmt.Sprintf("abandoned by @%s (confirmed)", ev.Author.Handle)
		return StatusCancelled, nil
	}

	// First tap — request confirmation.
	r.Object.AbandonRequestedAt = time.Now()
	body := fmt.Sprintf("⚠️ @%s requested to abandon this Run. **Are you sure?**\n\nReply `/everflow abandon` again within 12h to confirm; any other activity cancels.",
		ev.Author.Handle)
	if args != "" {
		body = fmt.Sprintf("%s\n\nReason: %s", body, args)
	}
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, body)
	return StatusAwaitingAbandonConfirm, nil
}

// cmdPause halts forward progress. Inbound webhook events while paused
// produce no transitions except via control verbs.
func (d *Deps) cmdPause(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	reason := fmt.Sprintf("paused by /everflow pause from @%s", ev.Author.Handle)
	if args != "" {
		reason = fmt.Sprintf("%s: %s", reason, args)
	}
	r.Object.PauseReason = reason
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
		fmt.Sprintf("🛑 Paused per @%s. Reply `/everflow resume` to continue.", ev.Author.Handle))
	return StatusPaused, nil
}

// cmdResume clears the paused state. The next inbound webhook will drive
// the next transition normally.
func (d *Deps) cmdResume(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	r.Object.PauseReason = ""
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
		fmt.Sprintf("▶️ Resumed per @%s. Watching for events.", ev.Author.Handle))
	return StatusAwaitingMerge, nil
}

// cmdSkip blacklists the current unit and closes its MR. Refactor moves on
// to the next unit (or completes if nothing remains).
func (d *Deps) cmdSkip(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	unitID := unitForMR(r.Object.InFlight, ev.MR)
	if unitID == "" {
		_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
			"`/everflow skip`: this MR isn't tracked by any active everflow Run.")
		return r.Status, nil
	}

	reason := fmt.Sprintf("skipped by /everflow skip from @%s", ev.Author.Handle)
	if args != "" {
		reason = fmt.Sprintf("%s: %s", reason, args)
	}

	_ = p.CloseMR(ctx, ev.MR.ProjectID, ev.MR.IID)
	next := d.markUnitBlacklisted(ctx, r, unitID, ev.MR, reason)
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
		fmt.Sprintf("⏭️ Skipped `%s` per @%s. MR closed; picking the next unit.", unitID, ev.Author.Handle))
	return next, nil
}

// cmdRetry clears PauseReason and unparks the Run. The author is responsible
// for re-triggering by event (re-comment, wait for CI rerun, etc.) — v1
// does not replay the last unsuccessful operation. Document this in the
// acknowledgement comment so the author knows what to do next.
func (d *Deps) cmdRetry(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	r.Object.PauseReason = ""
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
		fmt.Sprintf("🔄 Cleared pause per @%s. Re-comment your last review feedback or wait for CI to rerun to retry the underlying operation.", ev.Author.Handle))
	return StatusAwaitingMerge, nil
}

// cmdPrompt stores an extra instruction that the next runner invocation
// (in work() or invokeForEvent) will prepend to its prompt. Single slot —
// a second /prompt overrides the first until consumed.
func (d *Deps) cmdPrompt(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	if args == "" {
		_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
			"`/everflow prompt` needs text. Example:\n```\n/everflow prompt focus on the auth module first\n```")
		return r.Status, nil
	}
	r.Object.PromptInjection = args
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
		fmt.Sprintf("📝 Recorded prompt from @%s. Will inject into the next subagent call:\n```\n%s\n```", ev.Author.Handle, args))
	return r.Status, nil
}

// cmdStatus posts a one-comment summary of where the Run is.
func (d *Deps) cmdStatus(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, buildStatusComment(r))
	return r.Status, nil
}

func buildStatusComment(r *workflow.Run[AgentState, AgentStatus]) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Run `%s` status**\n\n", shortRunID(r.RunID))
	fmt.Fprintf(&b, "- Goal: %s\n", defaultIfEmpty(r.Object.Goal, "(none)"))
	fmt.Fprintf(&b, "- State: %s\n", r.Status)
	fmt.Fprintf(&b, "- Units: %d completed, %d blacklisted, %d in-flight, %d queued\n",
		len(r.Object.Completed), len(r.Object.Blacklisted),
		len(r.Object.InFlight), len(r.Object.Queue))
	fmt.Fprintf(&b, "- Subagent invocations: %d\n", r.Object.SubagentInvocations)
	fmt.Fprintf(&b, "- Tokens used: %d", r.Object.TotalTokens)
	if r.Object.Budget.MaxTokens > 0 {
		fmt.Fprintf(&b, " / %d", r.Object.Budget.MaxTokens)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Events seen: %d (skipped by filter: %d)\n",
		r.Object.EventsSeen, r.Object.EventsSkippedByFilter)
	if r.Object.PauseReason != "" {
		fmt.Fprintf(&b, "- Pause reason: %s\n", r.Object.PauseReason)
	}
	if r.Object.LastError != "" {
		fmt.Fprintf(&b, "- Last error: %s\n", r.Object.LastError)
	}
	if r.Object.PromptInjection != "" {
		fmt.Fprintf(&b, "- Pending prompt injection: yes\n")
	}
	return b.String()
}

// cmdStop terminates the Run. Closes all in-flight MRs, cleans up worktrees,
// posts a final comment, and transitions to StatusCancelled.
func (d *Deps) cmdStop(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]

	body := fmt.Sprintf("🛑 Stopped by `/everflow stop` from @%s. Closing in-flight MRs; run cancelled.", ev.Author.Handle)
	if args != "" {
		body = fmt.Sprintf("%s\n\nReason: %s", body, args)
	}
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, body)

	for unitID, mr := range r.Object.InFlight {
		_ = p.CloseMR(ctx, mr.ProjectID, mr.IID)
		d.cleanupWorktree(ctx, r, unitID)
	}

	r.Object.LastError = fmt.Sprintf("cancelled by /everflow stop from @%s", ev.Author.Handle)
	return StatusCancelled, nil
}

// cmdHelp responds to a bare "/everflow" with the verb menu. Returns the
// current status — no transition.
func (d *Deps) cmdHelp(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, helpMessage)
	return r.Status, nil
}

const helpMessage = "**everflow control verbs** (author only)\n\n" +
	"- `/everflow pause` — pause the Run\n" +
	"- `/everflow resume` — undo pause\n" +
	"- `/everflow skip [reason]` — blacklist this MR's unit and pick the next\n" +
	"- `/everflow retry` — clear pause; re-trigger by next webhook event\n" +
	"- `/everflow prompt <text>` — inject into the next subagent call\n" +
	"- `/everflow status` — post a progress summary\n" +
	"- `/everflow stop` — cancel the whole Run, close in-flight MRs (no confirmation)\n" +
	"- `/everflow abandon` — request abandonment with a 12h confirmation window\n" +
	"- `/everflow <anything else>` — treated as a freeform instruction for the subagent " +
	"(e.g. `/everflow refactor the auth module first`); requires this MR to be tracked by an in-flight unit\n"

// cmdFreeform handles a verb that isn't one of the recognised control
// commands. Rather than bouncing "Unknown command", the whole text after
// "/everflow " is treated as a freeform instruction for the subagent: it's
// stashed in PromptInjection (the same single-use slot /everflow prompt
// uses) and the event is replayed through invokeForEvent immediately, as if
// it were a NoteAdded event picked up by the filter. See ADR-0042.
//
// Requires the MR to be tracked by an in-flight unit — there's no subagent
// to direct otherwise, so this falls back to a "not tracked" reply, mirroring
// cmdSkip's guard.
func (d *Deps) cmdFreeform(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, verb string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	unitID := unitForMR(r.Object.InFlight, ev.MR)
	if unitID == "" {
		_ = postBotComment(ctx, r, p, ev.MR.ProjectID, ev.MR.IID,
			fmt.Sprintf("`/everflow %s`: this MR isn't tracked by any active everflow Run, so there's no subagent to direct. Reply `/everflow` for the verb list.", verb))
		return r.Status, nil
	}

	// Recompute from the raw body (not verb+args) to preserve original
	// casing and multi-line formatting — parseControlVerb lowercases verb.
	instruction := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ev.Note.Body), "/everflow"))
	r.Object.PromptInjection = instruction
	return d.invokeForEvent(ctx, r, unitID, ev)
}
