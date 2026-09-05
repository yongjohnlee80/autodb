package main

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
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
	// And the pin goes with it. A cleanup that left a second socket file
	// beside the first would trade one piece of litter for another.
	if _, serr := os.Stat(path + ".inode-pin"); !os.IsNotExist(serr) {
		t.Error("the inode pin outlived the socket it pinned")
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

// The pin's MECHANISM, asserted directly rather than through a defect that
// only some filesystems expose.
//
// The behavioural cells above are red on ext4 without the pin and green on
// tmpfs with or without it — which is precisely how this shipped. So this cell
// asserts the property that makes them sound wherever they run: while the pin
// is held, our inode is still ALLOCATED after the socket path is unlinked, and
// an allocated inode's number cannot be handed to a successor.
func TestPinSocket_KeepsTheInodeAllocated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autodb.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	t.Cleanup(func() { _ = ln.Close() })

	id, err := pinSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.pin == "" {
		t.Fatal("no pin was taken, so the identity is the unsound stat form — on a " +
			"filesystem that recycles inode numbers this daemon can delete a successor's socket")
	}
	pinned, err := os.Stat(id.pin)
	if err != nil {
		t.Fatalf("stat the pin: %v", err)
	}
	if !os.SameFile(id.stat, pinned) {
		t.Fatal("the pin does not name the inode we recorded, so holding it protects nothing")
	}
	if nl := nlinkOf(t, id.pin); nl != 2 {
		t.Fatalf("the socket has %d links, want 2 (itself and the pin)", nl)
	}

	// THE PROPERTY: unlink the socket and the inode survives, held by the pin.
	// That is what stops its number being recycled under a successor.
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}
	if nl := nlinkOf(t, id.pin); nl != 1 {
		t.Fatalf("after unlinking the socket the inode has %d links, want 1", nl)
	}
	still, err := os.Stat(id.pin)
	if err != nil {
		t.Fatalf("the pin vanished with the socket, so the inode was freed: %v", err)
	}
	if !os.SameFile(id.stat, still) {
		t.Fatal("the pin no longer names our inode")
	}

	id.release()
	if _, serr := os.Stat(id.pin); !os.IsNotExist(serr) {
		t.Error("release left the pin behind")
	}
}

// A stale pin from a crashed predecessor must not stop the next start from
// taking its own. We hold the bind, so no live daemon owns that name.
func TestPinSocket_ReplacesAStalePin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autodb.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable here: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	t.Cleanup(func() { _ = ln.Close() })

	stale := path + ".inode-pin"
	if werr := os.WriteFile(stale, []byte("a crashed predecessor's"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	id, err := pinSocket(path)
	if err != nil {
		t.Fatalf("a stale pin blocked the start: %v", err)
	}
	if id.pin == "" {
		t.Fatal("a stale pin silently downgraded us to the unsound stat identity")
	}
	pinned, serr := os.Stat(id.pin)
	if serr != nil {
		t.Fatal(serr)
	}
	if !os.SameFile(id.stat, pinned) {
		t.Fatal("the pin still names the predecessor's file, not our socket")
	}
	id.release()
}

func nlinkOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat of %s is %T, not a *syscall.Stat_t", path, fi.Sys())
	}
	return uint64(st.Nlink)
}
