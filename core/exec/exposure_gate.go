package exec

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// exposureGate serializes connection exposure transitions against front-door
// login finalizations. Readers are finalizations; the writer is
// SetConnectionExposure.
//
// WHY NOT sync.RWMutex. An RLock behind a writer blocks until that writer
// finishes, and nothing can abandon it. The writer holds the gate across a
// metadata read and transaction on its own caller's context, so a login waiting
// to finalize could outlive its own budget, and its client's departure, behind a
// stalled writer. Both acquisitions here take a context. Weighted serves waiters
// in FIFO order, so a waiting writer blocks later readers: the same precedence
// RWMutex gives, so exposure transitions cannot be starved.
type exposureGate struct{ sem *semaphore.Weighted }

// gateReaders is the writer's weight: more than any number of concurrent
// finalizations (each front-door connection is at most one).
const gateReaders = 1 << 20

func newExposureGate() *exposureGate {
	return &exposureGate{sem: semaphore.NewWeighted(gateReaders)}
}

// RLock takes the gate for one finalization, or returns ctx's error.
func (g *exposureGate) RLock(ctx context.Context) error { return g.sem.Acquire(ctx, 1) }

// RUnlock gives back one finalization's share.
func (g *exposureGate) RUnlock() { g.sem.Release(1) }

// Lock takes the whole gate for an exposure transition, or returns ctx's error.
func (g *exposureGate) Lock(ctx context.Context) error { return g.sem.Acquire(ctx, gateReaders) }

// Unlock gives the whole gate back.
func (g *exposureGate) Unlock() { g.sem.Release(gateReaders) }
