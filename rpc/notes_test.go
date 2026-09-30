package rpc_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

// notesFixture is a server keeping notes under a temp directory, with a
// workspace holding the fixture's connection, and alice, a reader granted on
// it: the workspace is one both root and alice can see.
type notesFixture struct {
	*fixture
	dir      string
	ws       int64
	aliceTok string
}

func newNotesFixture(t *testing.T) *notesFixture {
	t.Helper()
	dir := t.TempDir()
	f := newFixture(t, rpc.WithNotesDir(dir))
	ctx := context.Background()
	ws, err := f.eng.CreateWorkspace(ctx, f.rootTok, "ops", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.eng.AttachConnection(ctx, f.rootTok, ws, f.connID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	alice, err := f.svc.CreateUser(ctx, f.rootTok, "alice", "alice-passphrase-long", meta.RoleReader, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddGrant(ctx, f.rootTok, alice, f.connID, meta.RoleReader, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	c := f.session(t)
	return &notesFixture{fixture: f, dir: dir, ws: ws, aliceTok: c.login("alice", "alice-passphrase-long")}
}

func field(t *testing.T, res any, key string) any {
	t.Helper()
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("result %#v is not a map", res)
	}
	return m[key]
}

// Create, list, write against the version read, read back, and delete; a
// stale version and a "new" write over an existing note are conflicts.
func TestNotesRoundTripWithVersions(t *testing.T) {
	f := newNotesFixture(t)
	c := f.session(t)
	tok := f.aliceTok

	errVal, res := c.call("notes.create", tok, f.ws, "daily")
	if errVal != nil {
		t.Fatalf("create: %#v", errVal)
	}
	v0 := field(t, res, "version").(string)
	errVal, _ = c.call("notes.create", tok, f.ws, "daily")
	mustErr(t, errVal, rpc.CodeNoteConflict)

	errVal, res = c.call("notes.list", tok, f.ws)
	if errVal != nil || len(res.([]any)) != 1 || res.([]any)[0] != "daily.sql" {
		t.Fatalf("list: %#v %#v", errVal, res)
	}
	errVal, res = c.call("notes.write", tok, f.ws, "daily", "select 1;", v0)
	if errVal != nil {
		t.Fatalf("write at the created version: %#v", errVal)
	}
	v1 := field(t, res, "version").(string)
	errVal, _ = c.call("notes.write", tok, f.ws, "daily", "select 2;", v0)
	mustErr(t, errVal, rpc.CodeNoteConflict) // stale
	errVal, _ = c.call("notes.write", tok, f.ws, "daily", "select 2;", "")
	mustErr(t, errVal, rpc.CodeNoteConflict) // "new", but it exists

	errVal, res = c.call("notes.read", tok, f.ws, "daily")
	if errVal != nil || field(t, res, "body") != "select 1;" || field(t, res, "version") != v1 {
		t.Fatalf("read: %#v %#v", errVal, res)
	}
	errVal, res = c.call("notes.read", tok, f.ws, "absent")
	if errVal != nil || field(t, res, "body") != "" || field(t, res, "version") != "" {
		t.Fatalf("read of an absent note: %#v %#v; want empty at version \"\"", errVal, res)
	}
	errVal, res = c.call("notes.write", tok, f.ws, "fresh", "select 3;", "")
	if errVal != nil || field(t, res, "version") == "" {
		t.Fatalf("a new note written at version \"\": %#v %#v", errVal, res)
	}
	if errVal, _ := c.call("notes.delete", tok, f.ws, "daily"); errVal != nil {
		t.Fatalf("delete: %#v", errVal)
	}
	errVal, res = c.call("notes.list", tok, f.ws)
	if errVal != nil || len(res.([]any)) != 1 || res.([]any)[0] != "fresh.sql" {
		t.Fatalf("list after delete: %#v %#v", errVal, res)
	}
	for _, bad := range []string{"../escape", ".hidden", "a/b", ""} {
		errVal, _ = c.call("notes.read", tok, f.ws, bad)
		mustErr(t, errVal, -32602)
	}
}

// Notes are their owner's: alice's are under her own root, root cannot list
// or read them through any workspace id, and nothing names the subject but
// the token.
func TestNotesAreTheCallersOwn(t *testing.T) {
	f := newNotesFixture(t)
	c := f.session(t)
	if errVal, _ := c.call("notes.write", f.aliceTok, f.ws, "mine", "alice's", ""); errVal != nil {
		t.Fatalf("alice's write: %#v", errVal)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "u-alice", "ws-"+strconv.FormatInt(f.ws, 10), "mine.sql")); err != nil {
		t.Fatalf("alice's note is not under her own root: %v", err)
	}
	errVal, res := c.call("notes.list", f.rootTok, f.ws)
	if errVal != nil || len(res.([]any)) != 0 {
		t.Fatalf("root listing the same workspace: %#v %#v; want empty (root's own notes)", errVal, res)
	}
	errVal, res = c.call("notes.read", f.rootTok, f.ws, "mine")
	if errVal != nil || field(t, res, "version") != "" {
		t.Fatalf("root reading alice's note name: %#v %#v; want root's own (absent) note", errVal, res)
	}
	errVal, _ = c.call("notes.list", "not-a-token", f.ws)
	mustErr(t, errVal, rpc.CodeAuth)
}

// A folder whose workspace is gone is listed as detached; its notes can
// still be read, written and deleted by their owner, but no note can be
// created in it, and no new one written.
func TestADetachedFolderOverRPC(t *testing.T) {
	f := newNotesFixture(t)
	c := f.session(t)
	tok := f.aliceTok
	if errVal, _ := c.call("notes.write", tok, f.ws, "kept", "select 1;", ""); errVal != nil {
		t.Fatalf("write: %#v", errVal)
	}
	if err := f.eng.DeleteWorkspace(context.Background(), f.rootTok, f.ws, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	errVal, res := c.call("notes.workspaces", tok)
	if errVal != nil || len(res.([]any)) != 1 || field(t, res.([]any)[0], "detached") != true || field(t, res.([]any)[0], "id") != f.ws {
		t.Fatalf("workspaces: %#v %#v; want the folder, detached", errVal, res)
	}
	errVal, res = c.call("notes.read", tok, f.ws, "kept")
	if errVal != nil || field(t, res, "body") != "select 1;" {
		t.Fatalf("read in a detached folder: %#v %#v", errVal, res)
	}
	v := field(t, res, "version").(string)
	if errVal, _ = c.call("notes.write", tok, f.ws, "kept", "select 2;", v); errVal != nil {
		t.Fatalf("write of an existing note in a detached folder: %#v", errVal)
	}
	errVal, _ = c.call("notes.create", tok, f.ws, "another")
	mustErr(t, errVal, rpc.CodeWorkspaceNotVisible)
	errVal, _ = c.call("notes.write", tok, f.ws, "another", "x", "")
	mustErr(t, errVal, rpc.CodeWorkspaceNotVisible)
	if errVal, _ = c.call("notes.delete", tok, f.ws, "kept"); errVal != nil {
		t.Fatalf("delete in a detached folder: %#v", errVal)
	}
}

// A workspace the caller cannot see: no folder can be made in it by create
// or by a new write.
func TestANewNoteNeedsAVisibleWorkspace(t *testing.T) {
	f := newNotesFixture(t)
	c := f.session(t)
	errVal, _ := c.call("notes.create", f.aliceTok, int64(987654), "x")
	mustErr(t, errVal, rpc.CodeWorkspaceNotVisible)
	errVal, _ = c.call("notes.write", f.aliceTok, int64(987654), "x", "body", "")
	mustErr(t, errVal, rpc.CodeWorkspaceNotVisible)
	if _, err := os.Stat(filepath.Join(f.dir, "u-alice", "ws-987654")); !os.IsNotExist(err) {
		t.Fatalf("a folder was made for an invisible workspace: %v", err)
	}
}

// The store's refusals hold over RPC: a symlinked workspace folder and a
// symlinked note are refused, and nothing outside the caller's root is read.
func TestSymlinksAreRefusedOverRPC(t *testing.T) {
	f := newNotesFixture(t)
	c := f.session(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.sql"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if errVal, _ := c.call("notes.write", f.aliceTok, f.ws, "seed", "x", ""); errVal != nil {
		t.Fatalf("seed: %#v", errVal)
	}
	root := filepath.Join(f.dir, "u-alice")
	if err := os.Symlink(filepath.Join(outside, "secret.sql"), filepath.Join(root, "ws-"+strconv.FormatInt(f.ws, 10), "link.sql")); err != nil {
		t.Fatal(err)
	}
	errVal, res := c.call("notes.read", f.aliceTok, f.ws, "link")
	if errVal == nil {
		t.Fatalf("a symlinked note was read: %#v", res)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ws-424242")); err != nil {
		t.Fatal(err)
	}
	errVal, res = c.call("notes.read", f.aliceTok, int64(424242), "secret")
	if errVal == nil && field(t, res, "body") == "secret" {
		t.Fatal("a symlinked workspace folder was followed out of the caller's root")
	}
}
