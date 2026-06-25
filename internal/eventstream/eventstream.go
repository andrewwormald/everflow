// Package eventstream is a single-process workflow.EventStreamer where
// Recv parks on a sync.Cond instead of busy-spinning.
//
// The default in-memory adapter shipped with luno/workflow
// (adapters/memstreamer) implements Recv as an unconditional `for { ...
// continue }` loop with no backoff: when the log is exhausted, every
// consumer goroutine hammers two mutexes in a tight cycle. With ~7
// step consumers + a timeout consumer in everflow's refactor-sweep,
// that pegs all cores at ~380% CPU even when there's nothing to do.
// See ADR-0033.
//
// Semantics match memstreamer: in-memory log, topic filter, per-receiver
// cursor, StreamFromLatest option. The behavioural difference is that
// Recv blocks on cond.Wait until a Send broadcasts (or ctx is cancelled).
package eventstream

import (
	"context"
	"sync"
	"time"

	"github.com/luno/workflow"
)

// New returns a Streamer ready to wire into workflow.Build's EventStreamer
// slot. Durability comes from the RecordStore's transactional outbox —
// the streamer itself is intentionally in-memory and single-process.
func New() *Streamer {
	s := &Streamer{
		cursors: make(map[string]int),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Streamer fans out workflow events to per-step receivers. Safe for
// concurrent senders and receivers; one Streamer per daemon process.
type Streamer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	log     []*workflow.Event
	cursors map[string]int
}

var _ workflow.EventStreamer = (*Streamer)(nil)

func (s *Streamer) NewSender(_ context.Context, topic string) (workflow.EventSender, error) {
	return &sender{s: s, topic: topic}, nil
}

func (s *Streamer) NewReceiver(
	_ context.Context,
	topic string,
	name string,
	opts ...workflow.ReceiverOption,
) (workflow.EventReceiver, error) {
	var options workflow.ReceiverOptions
	for _, opt := range opts {
		opt(&options)
	}
	s.mu.Lock()
	if options.StreamFromLatest && s.cursors[name] == 0 {
		s.cursors[name] = len(s.log)
	}
	s.mu.Unlock()
	return &receiver{s: s, topic: topic, name: name}, nil
}

type sender struct {
	s     *Streamer
	topic string
}

func (snd *sender) Send(_ context.Context, foreignID string, statusType int, headers map[workflow.Header]string) error {
	s := snd.s
	s.mu.Lock()
	s.log = append(s.log, &workflow.Event{
		ID:        int64(len(s.log)) + 1,
		ForeignID: foreignID,
		Type:      statusType,
		Headers:   headers,
		CreatedAt: time.Now(),
	})
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

func (snd *sender) Close() error { return nil }

type receiver struct {
	s     *Streamer
	topic string
	name  string
}

func (r *receiver) Recv(ctx context.Context) (*workflow.Event, workflow.Ack, error) {
	s := r.s

	// sync.Cond doesn't natively respect ctx. Spawn a watcher that
	// broadcasts when the ctx is done so Recv can wake up and return
	// ctx.Err() instead of leaking the goroutine forever.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-stop:
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		cursor := s.cursors[r.name]
		for cursor < len(s.log) {
			e := s.log[cursor]
			if r.topic != e.Headers[workflow.HeaderTopic] {
				// Not for us; skip past it under the lock so other
				// receivers see the same cursor advance.
				cursor++
				s.cursors[r.name] = cursor
				continue
			}
			consumed := cursor
			ack := func() error {
				s.mu.Lock()
				if s.cursors[r.name] <= consumed {
					s.cursors[r.name] = consumed + 1
					s.cond.Broadcast() // future Recvs may want to advance past
				}
				s.mu.Unlock()
				return nil
			}
			return e, ack, nil
		}
		// No event waiting — park until Send or ctx cancel signals us.
		s.cond.Wait()
	}
}

func (r *receiver) Close() error { return nil }
