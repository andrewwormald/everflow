// Package store provides the durable record + timeout stores wired into
// the luno/workflow runtime. v1-scaffold uses the in-memory adapters; a
// follow-up commit swaps in sqlite-backed implementations (see DESIGN.md
// "What's not yet built" step 3).
package store

import (
	"github.com/luno/workflow"
	"github.com/luno/workflow/adapters/memrecordstore"
	"github.com/luno/workflow/adapters/memtimeoutstore"
)

// Open returns a (RecordStore, TimeoutStore) pair for the daemon to use.
// The signature lets us swap in sqlite later without touching callers.
func Open(_ string) (workflow.RecordStore, workflow.TimeoutStore, error) {
	return memrecordstore.New(), memtimeoutstore.New(), nil
}
