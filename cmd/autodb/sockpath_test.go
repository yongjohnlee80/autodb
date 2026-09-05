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
		created, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		removeIfStillOurs(path, created)
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
		created, err := os.Stat(path)
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
		if os.SameFile(created, successor) {
			t.Fatal("the fixture did not actually replace the file, so this cell would pass " +
				"whatever the code did")
		}

		removeIfStillOurs(path, created)

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
		created, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		removeIfStillOurs(path, created) // must not panic or recreate anything
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

	created, err := os.Stat(path)
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

	removeIfStillOurs(path, created)
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("our own socket survived shutdown")
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
	created, err := os.Stat(path)
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

	removeIfStillOurs(path, created)

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
