// Package store provides the durable record + timeout stores wired into
// the luno/workflow runtime. The sqlite backend (in this package) is the
// default; the in-memory adapters from luno/workflow are used only when
// path is empty (test harness).
package store

import (
	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memtimeoutstore"
)

// Open returns a (RecordStore, TimeoutStore) pair for the daemon to use.
// path == "" → in-memory adapters (volatile; for tests and the v0 scaffold).
// path != "" → sqlite Backend at that path (durable across restarts).
func Open(path string) (workflow.RecordStore, workflow.TimeoutStore, error) {
	if path == "" {
		return memrecordstore.New(), memtimeoutstore.New(), nil
	}
	b, err := OpenSqlite(path)
	if err != nil {
		return nil, nil, err
	}
	return b.RecordStore(), b.TimeoutStore(), nil
}
