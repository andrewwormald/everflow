// Package eventstream is a single-process workflow.EventStreamer backed by
// the sqlite event_log / event_cursors tables (see internal/store), so an
// in-flight event survives a daemon restart. Recv parks on a sync.Cond
// instead of busy-spinning — see ADR-0033.
//
// The default in-memory adapter shipped with luno/workflow
// (adapters/memstreamer) implements Recv as an unconditional `for { ...
// continue }` loop with no backoff: when the log is exhausted, every
// consumer goroutine hammers two mutexes in a tight cycle. With ~7
// step consumers + a timeout consumer in everflow's refactor-sweep,
// that pegs all cores at ~380% CPU even when there's nothing to do.
//
// Semantics match memstreamer: topic filter, per-receiver cursor,
// StreamFromLatest option. Two behavioural differences: Recv blocks on
// cond.Wait until a Send broadcasts (or ctx is cancelled), and the log +
// cursors live in sqlite rather than an in-process slice/map, so a daemon
// restart resumes every receiver from its last acked position instead of
// replaying (or losing) events. See ADR-0049.
package eventstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/luno/workflow"
)

// New returns a Streamer ready to wire into workflow.Build's EventStreamer
// slot. db must already have the event_log / event_cursors schema applied
// (internal/store.OpenSqlite does this) and is typically the same handle
// backing the daemon's RecordStore / TimeoutStore.
func New(db *sql.DB) *Streamer {
	s := &Streamer{db: db}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Streamer fans out workflow events to per-step receivers. Safe for
// concurrent senders and receivers; one Streamer per daemon process.
type Streamer struct {
	db   *sql.DB
	mu   sync.Mutex
	cond *sync.Cond
}

var _ workflow.EventStreamer = (*Streamer)(nil)

func (s *Streamer) NewSender(_ context.Context, topic string) (workflow.EventSender, error) {
	return &sender{s: s, topic: topic}, nil
}

func (s *Streamer) NewReceiver(
	ctx context.Context,
	topic string,
	name string,
	opts ...workflow.ReceiverOption,
) (workflow.EventReceiver, error) {
	var options workflow.ReceiverOptions
	for _, opt := range opts {
		opt(&options)
	}
	if err := s.ensureCursor(ctx, name, topic, options.StreamFromLatest); err != nil {
		return nil, fmt.Errorf("ensure cursor: %w", err)
	}
	return &receiver{s: s, topic: topic, name: name}, nil
}

// ensureCursor creates the receiver's cursor row on first sight only — if
// a row already exists (e.g. from before a daemon restart) its cursor is
// left untouched so the receiver resumes rather than replays or skips.
func (s *Streamer) ensureCursor(ctx context.Context, name, topic string, streamFromLatest bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM event_cursors WHERE name = ?`, name).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}

	var start int64
	if streamFromLatest {
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM event_log`).Scan(&start); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO event_cursors (name, topic, cursor, updated_at) VALUES (?, ?, ?, ?)`,
		name, topic, start, time.Now().UnixNano())
	return err
}

type sender struct {
	s     *Streamer
	topic string
}

func (snd *sender) Send(ctx context.Context, foreignID string, statusType int, headers map[workflow.Header]string) error {
	s := snd.s

	headersBlob, err := json.Marshal(headers)
	if err != nil {
		return fmt.Errorf("marshal headers: %w", err)
	}
	topic := headers[workflow.HeaderTopic]

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO event_log (topic, foreign_id, type, headers, created_at) VALUES (?, ?, ?, ?, ?)`,
		topic, foreignID, statusType, headersBlob, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	s.cond.Broadcast()
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
		e, err := s.nextEvent(ctx, r.name, r.topic)
		if err != nil {
			return nil, nil, err
		}
		if e != nil {
			consumed := e.ID
			// Detached from ctx: an ack must land even if the caller's ctx
			// is cancelled concurrently with (or just after) delivery,
			// otherwise a processed event's cursor never advances and it
			// is redelivered forever after restart.
			ackCtx := context.WithoutCancel(ctx)
			ack := func() error { return s.advanceCursor(ackCtx, r.name, consumed) }
			return e, ack, nil
		}
		s.cond.Wait()
	}
}

func (r *receiver) Close() error { return nil }

// logRow is one event_log row read back from sqlite.
type logRow struct {
	id      int64
	topic   string
	fid     string
	typ     int
	headers []byte
	created int64
}

// nextEvent returns the next event on topic past name's cursor, or nil if
// none is available yet. Rows for other topics are skipped by advancing
// the persisted cursor past them (without waiting for an Ack, mirroring
// memstreamer semantics), so a slow receiver on one topic never blocks a
// fast receiver on another sharing the same log. Must be called with
// s.mu held.
func (s *Streamer) nextEvent(ctx context.Context, name, topic string) (*workflow.Event, error) {
	var cursor int64
	if err := s.db.QueryRowContext(ctx, `SELECT cursor FROM event_cursors WHERE name = ?`, name).Scan(&cursor); err != nil {
		return nil, fmt.Errorf("load cursor: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id, topic, foreign_id, type, headers, created_at
FROM event_log WHERE id > ? ORDER BY id ASC`, cursor)
	if err != nil {
		return nil, err
	}
	var candidates []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.id, &r.topic, &r.fid, &r.typ, &r.headers, &r.created); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var match *logRow
	skipTo := cursor
	for i := range candidates {
		if candidates[i].topic != topic {
			skipTo = candidates[i].id
			continue
		}
		match = &candidates[i]
		break
	}
	if skipTo != cursor {
		if _, err := s.db.ExecContext(ctx, `
UPDATE event_cursors SET cursor = ?, updated_at = ? WHERE name = ? AND cursor < ?`,
			skipTo, time.Now().UnixNano(), name, skipTo); err != nil {
			return nil, err
		}
	}
	if match == nil {
		return nil, nil
	}

	var headers map[workflow.Header]string
	if len(match.headers) > 0 {
		if err := json.Unmarshal(match.headers, &headers); err != nil {
			return nil, fmt.Errorf("unmarshal headers: %w", err)
		}
	}
	return &workflow.Event{
		ID:        match.id,
		ForeignID: match.fid,
		Type:      match.typ,
		Headers:   headers,
		CreatedAt: time.Unix(0, match.created),
	}, nil
}

// advanceCursor persists an Ack and wakes parked receivers so they can
// re-check whether they're now unblocked (e.g. two receivers sharing a
// name — not a normal configuration, but harmless to signal).
func (s *Streamer) advanceCursor(ctx context.Context, name string, consumed int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
UPDATE event_cursors SET cursor = ?, updated_at = ? WHERE name = ? AND cursor < ?`,
		consumed, time.Now().UnixNano(), name, consumed)
	if err != nil {
		return err
	}
	s.cond.Broadcast()
	return nil
}
