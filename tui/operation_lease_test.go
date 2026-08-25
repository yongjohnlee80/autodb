package tui

// The operation LEASE (ADR-0068 §2.2, lector r1 finding 4).
//
// A retirement flag only stops operations that have not started. The previous
// version checked `alive()` and released its mutex before touching the
// filesystem, so Retire could return while an admitted Save was still writing —
// and the caller would install the next identity believing the previous one had
// finished. "Retired" has to mean "no effect of mine is still in progress".

import (
	"errors"
	"testing"
	"time"
)

func TestRetireWaitsForAdmittedOperations(t *testing.T) {
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	// An operation is admitted and still running.
	if err := ns.begin(); err != nil {
		t.Fatal(err)
	}

	returned := make(chan struct{})
	go func() { ns.Retire(); close(returned) }()

	select {
	case <-returned:
		t.Fatal("Retire returned while an admitted operation was still in flight — " +
			"the next identity would be installed on top of a write still happening")
	case <-time.After(100 * time.Millisecond):
		// still waiting, which is the contract
	}

	ns.end() // the in-flight operation completes

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Retire never returned after the admitted operation finished")
	}
}

// New work is refused as soon as retirement STARTS, even while it waits for the
// drain — otherwise a caller could slip an operation in behind the barrier.
func TestRetireRefusesNewWorkWhileDraining(t *testing.T) {
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ns.begin(); err != nil {
		t.Fatal(err)
	}
	go ns.Retire()
	// Give retirement time to set the flag and block on the drain.
	time.Sleep(50 * time.Millisecond)

	if err := ns.begin(); !errors.Is(err, ErrRetired) {
		if err == nil {
			ns.end()
		}
		t.Fatalf("a new operation was admitted while retirement was draining: %v", err)
	}
	ns.end()
}

// The paired positive: an un-retired store admits work normally. Without it,
// refusing everything would satisfy both controls above.
func TestLiveStoreAdmitsWork(t *testing.T) {
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := ns.begin(); err != nil {
			t.Fatalf("live store refused admission: %v", err)
		}
		ns.end()
	}
	note, _, err := ns.Load(1, "draft.sql")
	if err != nil {
		t.Fatalf("live store refused Load: %v", err)
	}
	if err := ns.Save(note, "body"); err != nil {
		t.Fatalf("live store refused Save: %v", err)
	}
}

// Every public operation is bracketed. Enumerated rather than sampled, because
// the previous version of this assertion listed the ones I remembered and
// ListWorkspaceDirs was not among them (lector).
func TestEveryPublicOperationIsRefusedAfterRetirement(t *testing.T) {
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := ns.Load(1, "draft.sql")
	if err != nil {
		t.Fatal(err)
	}
	ns.Retire()

	for name, op := range map[string]func() error{
		"List":              func() error { _, e := ns.List(1); return e },
		"Load":              func() error { _, _, e := ns.Load(1, "draft.sql"); return e },
		"Save":              func() error { return ns.Save(note, "x") },
		"Create":            func() error { _, e := ns.Create(1, "new.sql"); return e },
		"Delete":            func() error { return ns.Delete(1, "draft.sql") },
		"ListWorkspaceDirs": func() error { _, e := ns.ListWorkspaceDirs(); return e },
	} {
		if err := op(); !errors.Is(err, ErrRetired) {
			t.Errorf("%s served a retired store: %v", name, err)
		}
	}
}
