package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Shutdown must not delete a SUCCESSOR's socket.
//
// The bug this pins was reachable by an ordinary restart — pkill followed
// immediately by a start — and its symptom is the worst kind: `ss` reports a
// healthy listener on an orphaned inode, `ls` shows nothing, and every client
// fails to dial with no error anywhere saying why.
//
// Both halves are required. The removal half alone passes for the original
// unconditional os.Remove, which is exactly the defect.
func TestRemoveIfStillOurs(t *testing.T) {
	t.Run("removes the file it created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "autodb.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := pinSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		removeIfStillOurs(path, id)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("our own socket file survived shutdown; the next launch will look occupied " +
				"while nothing is listening")
		}
	})

	t.Run("leaves a successor's file alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "autodb.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := pinSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		// A successor binds the same PATH — a different file wearing our name.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("successor"), 0o600); err != nil {
			t.Fatal(err)
		}
		successor, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// The premise guard that caught the real defect. Before the inode was
		// pinned this FAILED on ext4 — the successor was handed our recycled
		// inode number, so os.SameFile called two different files the same and
		// the cell could not have observed anything. It is the pin that makes
		// the premise true rather than the filesystem.
		if os.SameFile(id.stat, successor) {
			t.Fatal("the fixture did not actually replace the file, so this cell would pass " +
				"whatever the code did")
		}

		removeIfStillOurs(path, id)

		if _, err := os.Stat(path); err != nil {
			t.Fatal("shutdown deleted the SUCCESSOR's socket: ss would report a healthy listener " +
				"on an orphaned inode while every client fails to dial")
		}
	})

	t.Run("tolerates the file already being gone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "autodb.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := pinSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		removeIfStillOurs(path, id) // must not panic or recreate anything
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("the path came back")
		}
	})
}

// SF3: the helper cell above drives regular files and never constructs a
// listener, so it could not see that the stdlib was doing the removal.
//
// This exercises the REAL sequence — bind, Close, defer — and pins the
// SetUnlinkOnClose(false) that routes every removal through the identity check.
func TestSocketPath_TheRealShutdownSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autodb.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T, not a *net.UnixListener", ln)
	}
	ul.SetUnlinkOnClose(false)

	id, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if cerr := ln.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	// The point of SetUnlinkOnClose(false): the file is STILL THERE after
	// Close, so our own removal is the one that runs. Left at the default the
	// stdlib would have unlinked it by name and the identity check below would
	// be dead code taking an early return.
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("Close() removed the socket by name, so the identity check never runs: %v", serr)
	}

	removeIfStillOurs(path, id)
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("our own socket survived shutdown")
	}
	// AND NOTHING ELSE IS LEFT IN THE DIRECTORY. The first version of the
	// hold was a hard link named "<sock>.inode-pin", which a reviewer showed
	// cancels itself: a successor binding the same path computes the same name
	// and removes the predecessor's. An on-disk artifact derived from the
	// socket path is the whole bug, so the cell asserts there is no artifact
	// at all rather than that this particular name is cleaned up.
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range ents {
		t.Errorf("shutdown left %q beside the socket; an inode hold must have no name on "+
			"disk, or a successor sharing the path shares the artifact too", e.Name())
	}
}

// The sequence that motivates the whole fix: we close, a SUCCESSOR binds the
// same path, and only then does our deferred cleanup run.
func TestSocketPath_ASuccessorBindingAfterOurCloseSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autodb.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	id, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}

	successor, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("successor could not bind: %v", err)
	}
	if sul, ok := successor.(*net.UnixListener); ok {
		sul.SetUnlinkOnClose(false)
	}
	t.Cleanup(func() { _ = successor.Close() })

	removeIfStillOurs(path, id)

	if _, serr := os.Stat(path); serr != nil {
		t.Fatal("our shutdown deleted the SUCCESSOR's socket: ss would report a healthy listener " +
			"on an orphaned inode while every client fails to dial")
	}
	// And the successor is genuinely still usable, not merely present.
	c, derr := net.Dial("unix", path)
	if derr != nil {
		t.Fatalf("the successor's socket is present but not dialable: %v", derr)
	}
	_ = c.Close()
}

// The hold's MECHANISM, asserted directly rather than through a defect that
// only some filesystems expose.
//
// The behavioural cells above are red on ext4 without a hold and green on tmpfs
// with or without one — which is precisely how the original shipped. So this
// cell asserts the property that makes them sound wherever they run.
func TestPinSocket_HoldsTheInodeNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autodb.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	id, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.hold == nil {
		t.Fatal("no inode hold was taken, so the identity is the unsound stat form — on a " +
			"filesystem that recycles inode numbers this daemon can delete a successor's socket")
	}
	// NO NAME ON DISK. This is the property the hard-link version did not have
	// and could not have: two processes on one path derived one pin name.
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 1 || ents[0].Name() != "autodb.sock" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("taking the hold created something on disk: %v", names)
	}

	// THE PROPERTY, on a filesystem that recycles: unlink the socket, let a
	// successor bind the same path, and the successor must NOT be handed our
	// inode number while we hold it.
	//
	// The cell states which case it is in rather than reporting a pass it did
	// not earn: where nothing recycles, the second half observes nothing, and
	// saying so is the difference between evidence and a green tick.
	ourStat := id.stat
	_ = ln.Close()
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}
	succ, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("successor could not bind: %v", err)
	}
	if sul, ok := succ.(*net.UnixListener); ok {
		sul.SetUnlinkOnClose(false)
	}
	t.Cleanup(func() { _ = succ.Close() })
	succStat, serr := os.Stat(path)
	if serr != nil {
		t.Fatal(serr)
	}
	if os.SameFile(ourStat, succStat) {
		t.Fatal("a successor was handed our inode number while the hold was open; the hold " +
			"is not holding anything and shutdown can delete the successor's socket")
	}
	if !recyclesInodeNumbers(t) {
		t.Log("NOTE: this filesystem does not recycle inode numbers, so the assertion above " +
			"passes without observing anything. It is a real measurement only on ext4 and " +
			"similar; see validate-the-verifier rule 9.")
	}
}

// recyclesInodeNumbers reports whether THIS filesystem hands a freed inode
// number straight back — the positive control for every assertion above.
//
// It exists so a cell can say "green here, and here cannot see it" instead of
// "green", which is the distinction the whole socket defect turned on.
func recyclesInodeNumbers(t *testing.T) bool {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return false
	}
	a, err := os.Stat(p)
	if err != nil {
		return false
	}
	if err := os.Remove(p); err != nil {
		return false
	}
	if err := os.WriteFile(p, []byte("successor"), 0o600); err != nil {
		return false
	}
	b, err := os.Stat(p)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// TWO daemons on ONE path must not share an artifact.
//
// This is the reviewer's finding as a cell. With a hard link both derived
// "<sock>.inode-pin" from the path, so the successor's setup destroyed the
// predecessor's guarantee and the predecessor's teardown destroyed the
// successor's. An open descriptor is per-process by construction; the cell
// pins that by holding both at once and releasing one.
func TestPinSocket_SuccessionDoesNotShareAnArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autodb.sock")

	lnA, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	lnA.(*net.UnixListener).SetUnlinkOnClose(false)
	idA, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if idA.hold == nil {
		t.Fatal("A took no hold; nothing below is observable")
	}

	// A stops serving and a successor takes the path, exactly as an immediate
	// restart does.
	_ = lnA.Close()
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}
	lnB, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("successor could not bind: %v", err)
	}
	lnB.(*net.UnixListener).SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = lnB.Close() })
	idB, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if idB.hold == nil {
		t.Fatal("B took no hold — a successor's setup must not be affected by a predecessor")
	}

	// B's arrival did not disturb A's identity, and A's shutdown does not
	// disturb B's socket.
	if os.SameFile(idA.stat, idB.stat) && recyclesInodeNumbers(t) {
		t.Error("A and B have the same identity while A still holds its inode")
	}
	removeIfStillOurs(path, idA)
	if _, serr := os.Stat(path); serr != nil {
		t.Fatal("A's shutdown deleted the SUCCESSOR's socket")
	}
	c, derr := net.Dial("unix", path)
	if derr != nil {
		t.Fatalf("the successor's socket is present but not dialable: %v", derr)
	}
	_ = c.Close()

	// And B is still whole: its own shutdown must still be able to identify
	// its socket, which the shared-name version destroyed.
	removeIfStillOurs(path, idB)
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("B could no longer identify its own socket at shutdown")
	}
}
