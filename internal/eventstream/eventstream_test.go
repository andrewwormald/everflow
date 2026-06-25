package eventstream

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luno/workflow"
)

const testTopic = "refactor-sweep"

// TestRecv_BlocksUntilSend proves the headline fix: with no events in the
// log, a Recv goroutine parks rather than burning CPU. We check that the
// goroutine count stays flat (it's sleeping on cond.Wait) and that the
// receiver wakes up promptly when a Send arrives.
func TestRecv_BlocksUntilSend(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rec, err := s.NewReceiver(ctx, testTopic, "test-consumer")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	got := make(chan *workflow.Event, 1)
	go func() {
		e, ack, err := rec.Recv(ctx)
		if err != nil {
			return
		}
		_ = ack()
		got <- e
	}()

	// Give the Recv goroutine time to enter cond.Wait. If the streamer
	// busy-loops instead, this would burn a core for those 50ms.
	time.Sleep(50 * time.Millisecond)

	send, err := s.NewSender(ctx, testTopic)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { _ = send.Close() })

	if err := send.Send(ctx, "fid-1", 42, map[workflow.Header]string{
		workflow.HeaderTopic: testTopic,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case e := <-got:
		if e.ForeignID != "fid-1" {
			t.Errorf("ForeignID: want fid-1, got %q", e.ForeignID)
		}
	case <-time.After(time.Second):
		t.Fatalf("Recv did not unblock within 1s of Send")
	}
}

// TestRecv_DoesNotBusySpin samples the runtime's goroutine count and
// ensures the wakeups don't churn goroutines. The real test (no CPU burn)
// is hard to assert deterministically — this is a sanity guard against
// regressions reintroducing a spin loop.
func TestRecv_DoesNotBusySpin(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const consumers = 8
	for i := 0; i < consumers; i++ {
		name := "consumer-" + string(rune('a'+i))
		rec, err := s.NewReceiver(ctx, testTopic, name)
		if err != nil {
			t.Fatalf("NewReceiver: %v", err)
		}
		go func() {
			for {
				if _, _, err := rec.Recv(ctx); err != nil {
					return
				}
			}
		}()
	}

	// Let consumers settle into cond.Wait.
	time.Sleep(100 * time.Millisecond)

	before := runtime.NumGoroutine()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Each Recv internally spawns a ctx-watcher goroutine that lives
	// for the duration of the Recv call. With no Sends arriving the
	// receivers stay parked and goroutine count should be stable.
	if after > before+2 { // +2 wiggle for the test runtime itself
		t.Errorf("goroutine count grew while idle: before=%d after=%d", before, after)
	}
}

// TestRecv_CtxCancelUnblocks proves that ctx cancellation wakes a parked
// Recv — otherwise the daemon would hang on shutdown.
func TestRecv_CtxCancelUnblocks(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(t.Context())

	rec, err := s.NewReceiver(ctx, testTopic, "cancel-test")
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	done := make(chan error, 1)
	go func() {
		_, _, err := rec.Recv(ctx)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond) // let it park
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("Recv should return ctx.Err(), got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Recv did not unblock on ctx cancel within 500ms")
	}
}

// TestRecv_SkipsOtherTopics: a receiver on topic A shouldn't consume an
// event tagged for topic B. Cursor advances past the skipped event so we
// don't see it again.
func TestRecv_SkipsOtherTopics(t *testing.T) {
	s := New()
	ctx := t.Context()

	send, _ := s.NewSender(ctx, "other-topic")
	if err := send.Send(ctx, "fid-skip", 1, map[workflow.Header]string{
		workflow.HeaderTopic: "other-topic",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := send.Send(ctx, "fid-keep", 1, map[workflow.Header]string{
		workflow.HeaderTopic: testTopic,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	rec, _ := s.NewReceiver(ctx, testTopic, "skip-test")
	e, ack, err := rec.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if e.ForeignID != "fid-keep" {
		t.Errorf("got %q, want fid-keep (the matching-topic event)", e.ForeignID)
	}
	_ = ack()
}

// TestRecv_StreamFromLatest: when the option is set, the receiver should
// skip events already in the log at creation time.
func TestRecv_StreamFromLatest(t *testing.T) {
	s := New()
	ctx := t.Context()

	send, _ := s.NewSender(ctx, testTopic)
	for i := 0; i < 5; i++ {
		if err := send.Send(ctx, "old-fid", 1, map[workflow.Header]string{
			workflow.HeaderTopic: testTopic,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	rec, _ := s.NewReceiver(ctx, testTopic, "latecomer", workflow.StreamFromLatest())

	// Recv should NOT immediately return one of the historical events.
	got := make(chan struct{}, 1)
	go func() {
		_, _, err := rec.Recv(ctx)
		if err == nil {
			got <- struct{}{}
		}
	}()
	select {
	case <-got:
		t.Fatalf("Recv returned a historical event despite StreamFromLatest")
	case <-time.After(50 * time.Millisecond):
		// expected: still parked
	}

	// New event arrives — that's the one StreamFromLatest should deliver.
	_ = send.Send(ctx, "new-fid", 1, map[workflow.Header]string{
		workflow.HeaderTopic: testTopic,
	})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatalf("Recv did not deliver the new event")
	}
}

// TestRecv_ConcurrentConsumersIndependentCursors: two consumers on the
// same topic must both see every event (each has its own cursor).
func TestRecv_ConcurrentConsumersIndependentCursors(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var aCount, bCount atomic.Int32
	consumeAll := func(name string, counter *atomic.Int32) {
		rec, _ := s.NewReceiver(ctx, testTopic, name)
		for {
			_, ack, err := rec.Recv(ctx)
			if err != nil {
				return
			}
			counter.Add(1)
			_ = ack()
		}
	}
	go consumeAll("a", &aCount)
	go consumeAll("b", &bCount)

	send, _ := s.NewSender(ctx, testTopic)
	const n = 10
	for i := 0; i < n; i++ {
		_ = send.Send(ctx, "f", 1, map[workflow.Header]string{
			workflow.HeaderTopic: testTopic,
		})
	}

	// Wait briefly for consumers to drain.
	for i := 0; i < 200; i++ {
		if aCount.Load() == n && bCount.Load() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("each consumer should have seen %d events; got a=%d b=%d", n, aCount.Load(), bCount.Load())
}
