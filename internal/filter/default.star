# Default everflow comment filter.
#
# Called once per inbound note_added / pipeline_failed event. Returns one of:
#   "skip"            — ignore the event; no LLM call
#   "invoke_subagent" — run the configured runner to handle the event
#   "pause"           — pause the Run for author intervention
#
# Inputs:
#   event   — dict; keys include kind, mr (iid), author (handle, is_bot),
#             is_author (bool), note (id, body), pipeline (id, status,
#             failed_jobs[]).
#   state   — read-only view of the Run: goal, mode, completed_count,
#             blacklisted_count, in_flight_count, queue_count.
#   phrases — call phrases.contains(text) to check the per-Run + global
#             skip list. Add phrases via the runner's Learnings.add_phrases
#             return value, not from inside this filter.
#
# Edit this file freely — the daemon re-reads it on every event. Bad syntax
# falls back to "invoke_subagent" with a log entry.

def filter(event, state, phrases):
    # Pipeline failures always reach the subagent — classifying CI failures
    # is exactly the kind of "needs reasoning" judgement we want it for.
    if event["kind"] == "pipeline_failed":
        return "invoke_subagent"

    # Below here: note_added events.
    body = event["note"]["body"].strip()
    body_lc = body.lower()

    # Author's /everflow ... commands are routed in resume() BEFORE this
    # filter runs, so we don't need to handle them here.

    # Bots: silent skip. Future: per-bot dispatch (Danger title check, etc.).
    if event["author"]["is_bot"]:
        return "skip"

    # Emoji-only or trivially short.
    if len(body) <= 3:
        return "skip"

    # Known phrases — cheap deterministic skip. Add to per-Run / global
    # YAML files (or via the runner's Learnings.add_phrases) to grow this.
    if phrases.contains(body_lc):
        return "skip"

    # First N words match a known phrase? Catches "lgtm 👍" style.
    words = body_lc.split()
    if len(words) <= 3 and phrases.contains(" ".join(words)):
        return "skip"

    # Anything else: reach for the subagent.
    return "invoke_subagent"
