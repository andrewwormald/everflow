# ADR-0033: In-process EventStreamer with cond.Wait, not luno/workflow's memstreamer

**Status**: Accepted
**Date**: 2026-06-24

## Context

Profiling the first end-to-end spike showed the daemon pinned at ~380 %
CPU for the entire 15-hour run, even after the single in-flight Run had
terminated and the outbox was empty. A SIGQUIT goroutine dump
attributed the load to six step-consumer goroutines all sitting at
`runnable` state inside
`luno/workflow/adapters/memstreamer.(*Stream).Recv` →
`(*cursorStore).Get`. The recv loop:

```go
for ctx.Err() == nil {
    s.mu.Lock()
    log := *s.log
    s.mu.Unlock()
    cursorOffset := s.cursorStore.Get(s.name)
    if len(log)-1 < cursorOffset {
        continue            // <-- no backoff; spin straight back to mutex
    }
    ...
}
```

There is no sleep, no `select`, no condition variable — when the log is
exhausted, every consumer hammers the streamer mutex plus the cursor
mutex in a tight loop. With ~7 step consumers + a timeout consumer in
the refactor-sweep workflow, the eight goroutines burn whatever cores
the scheduler will give them.

`luno/workflow@v0.4.0` ships only memory-only adapters
(`memstreamer`, `memrecordstore`, `memrolescheduler`, `memtimeoutstore`)
— they're labelled as test fixtures, but `everflow` was using
`memstreamer` for the production daemon because no other in-process
streamer existed and durability already comes from our sqlite-backed
transactional outbox (ADR-0022).

## Decision

`internal/eventstream` replaces `memstreamer` for the daemon's
`EventStreamer` slot. It is API-compatible with the workflow interface
and behaviourally compatible with memstreamer (in-memory log, topic
filter, per-receiver cursor, `StreamFromLatest` option) — the only
substantive difference is that `Recv` parks on a `sync.Cond` instead of
busy-spinning. `Send` `Broadcast`s the cond; `ctx` cancellation wakes
parked receivers via a per-Recv watcher goroutine.

The daemon will, in steady state, consume zero CPU when idle instead of
saturating multiple cores.

## Alternatives considered

- **Patch luno/workflow upstream.** Right thing to do eventually
  (the bug is in their public adapter), but turnaround time is days/weeks
  and we want the spike to ship today. Filed as a follow-up.
- **`replace` directive pointing at the local
  `/Users/andreww/dev/workflow` checkout.** Tight coupling between
  everflow and a personal in-progress branch; would surprise any other
  contributor cloning the repo. Rejected.
- **Add a 1–10 ms `time.Sleep` to the busy loop via a wrapper.** Bounds
  the damage but still wakes the goroutine ~100 ×/s per consumer and
  still hits the cursor mutex on every cycle. Cond signalling is
  trivially cheaper and the same code complexity.
- **Build a sqlite-backed streamer that polls the outbox.** Right answer
  for multi-process deployments (the daemon could restart and pick up
  events from disk), but everflow is single-process for the foreseeable
  future and the outbox already provides durability through the
  RecordStore. Doing both adds latency for no benefit. Revisit when
  we ship a multi-daemon topology.

## Consequences

- The daemon is no longer noisy on idle laptops; battery / fans /
  thermal headroom all benefit. The 380 % CPU regression we caught in
  the spike would have been a serious silent cost on production
  developer machines.
- We now own one more piece of luno/workflow surface. If `EventStreamer`
  / `EventReceiver` / `ReceiverOption` evolve upstream, our streamer
  needs to track those changes. The interface is small (3 methods
  total) and has been stable across `v0.x` releases, so the maintenance
  cost is low — but it isn't zero.
- The signalling is broadcast-based: every Send wakes every parked
  receiver, regardless of topic. With ~10 consumers and topic filtering
  on the receive side this is fine; if we ever fan out to hundreds of
  topics we should switch to per-topic conds. Not a near-term concern.
- The `ctx` watcher goroutine spawned inside each `Recv` is cleaned up
  via a `defer close(stop)` channel. We never leak it; benchmarks show
  the per-Recv overhead is ≪ a microsecond on Apple Silicon.
- Tests live alongside the implementation: a 200 ms "no goroutine growth
  while idle" smoke check serves as a regression guard against
  reintroducing a busy loop.
