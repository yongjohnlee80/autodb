package main

import (
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
