package tui

// The identity EPOCH (ADR-0068 §2.2) — lector's reproduced probe.
//
// The epoch existed but nothing read it, which is worse than not having one: it
// reads as protection. noteGen alone could not tell the difference, because
// retirement does not advance it — so a load ISSUED as alice could still match
// and repaint BOB's editor with alice's body.

import (
	"testing"
)

func TestDelayedLoadFromAPreviousIdentityIsDiscarded(t *testing.T) {
	m := unconnected()
	base := t.TempDir()
	alice, err := NewPersonalNotes(base, "alice")
	if err != nil {
		t.Fatal(err)
	}
	m.notes = alice

	// A load issued as alice, captured the way doOpenNote captures it.
	cap, ok := m.captureNotes()
	if !ok {
		t.Fatal("no capability for alice")
	}
	m.noteGen++
	gen := m.noteGen
	delayed := noteLoaded{epoch: cap.epoch, gen: gen, body: "alice secret"}

	// Alice goes away; bob arrives.
	m.retireIdentity()
	bob, err := NewPersonalNotes(base, "bob")
	if err != nil {
		t.Fatal(err)
	}
	m.notes = bob
	m.editor.SetValue("")

	// Alice's load lands now.
	m.handleTask(taskResultOf(delayed))

	if got := m.editor.Value(); got != "" {
		t.Errorf("bob's editor was repainted with a previous identity's note: %q", got)
	}
}

// The paired POSITIVE: a load issued under the CURRENT identity IS applied.
// Without it, discarding everything would satisfy the control above and opening
// a note would simply stop working.
//
// Mounted, unlike the negative: the negative returns before touching the
// context, so it passes on an unmounted Model — which would have hidden a fence
// that rejected everything.
func TestLoadFromTheCurrentIdentityIsApplied(t *testing.T) {
	m, _, sync := mounted(t)
	ns, err := NewPersonalNotes(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	sync(func() {
		m.notes = ns
		cap, _ := m.captureNotes()
		m.noteGen++
		note, _, _ := ns.Load(1, "draft.sql")
		m.editor.SetValue("")
		m.handleTask(taskResultOf(noteLoaded{
			epoch: cap.epoch, gen: m.noteGen, note: note, body: "alice's own note",
		}))
		got = m.editor.Value()
	})
	if got != "alice's own note" {
		t.Errorf("a current-identity load was not applied: %q", got)
	}
}
