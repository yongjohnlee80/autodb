package autodocplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// End to end, against a real `autodb --serve` built from this checkout and the scratch
// PostgreSQL (TEST_PGURL): the plugin finds the binary on PATH, starts its server, signs in,
// picks the connection, runs a query of 1234 rows and pages it to its end through the table, every
// row once and in order; a second run is a new table, and the server is left with no open cursor.
func TestLiveAQueryPagedToItsEnd(t *testing.T) {
	pgurl := os.Getenv("TEST_PGURL")
	if pgurl == "" {
		t.Skip("TEST_PGURL not set; skipping the live end-to-end cell")
	}
	ctx := context.Background()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	// Built in AutoDB's own module, four levels up: its go.sum, not this nested module's, holds
	// the server's dependencies, and a build from here resolves only what the plugin imports.
	repo, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "cmd", "autodb")); err != nil {
		t.Fatalf("AutoDB's checkout is not at %s: %v", repo, err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "autodb"), "./cmd/autodb")
	build.Dir = repo
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building autodb: %v", err)
	}
	for _, d := range []string{"config", "data", "state", "run", "plugin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("AUTODOC_PLUGIN_DIR", filepath.Join(root, "plugin"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := New("dev")
	p.inline = true // each step at once, on this goroutine
	h := &fakeHost{}
	p.attach(h)
	if p.step != stepLogin {
		t.Fatalf("the plugin did not reach the sign-in: step %d\n%s", p.step, said(p))
	}
	if p.bin.Via != "PATH" || p.bin.Path != filepath.Join(bin, "autodb") {
		t.Fatalf("found %+v, want the build on PATH", p.bin)
	}

	// The store is new: its first administrator, and the scratch connection, made as an operator
	// would, over the server's own wire.
	ep, err := autodb.PrintEndpoint(ctx, filepath.Join(bin, "autodb"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := golibrpc.Dial(ctx, ep.Addr, msgpackrpc.New(nil), golibrpc.ClientNetwork(ep.Network))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Call(ctx, "sys.hello", map[string]any{"protocol": autodb.Protocol}); err != nil {
		t.Fatal(err)
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	pass := "Pp-" + hex.EncodeToString(b[:]) // made for this run, never written down
	res, err := admin.Call(ctx, "auth.bootstrap", "admin", pass)
	if err != nil {
		t.Fatalf("auth.bootstrap: %v", err)
	}
	adminTok, _ := res.(map[string]any)["token"].(string)
	t.Cleanup(func() {
		// the server was started detached: stop it
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Call(sctx, "sys.shutdown", adminTok)
	})
	created, err := admin.Call(ctx, "conn.create", adminTok, "scratch", "postgres", pgurl)
	if err != nil {
		t.Fatalf("conn.create: %v", err)
	}

	signIn(p, pass)
	if p.step != stepPick {
		t.Fatalf("after signing in: step %d\n%s", p.step, said(p))
	}

	// The sign-in is recorded under the temporary state directory, keyed by the store the server
	// serves, the name alone; the next start of the plugin opens on it, the passphrase focused.
	if p.cli.Hello.StoreID == "" {
		t.Fatal("the server's hello names no store")
	}
	logins := filepath.Join(root, "state", "autodoc-autodb", "logins.json")
	if p.logins != logins {
		t.Fatalf("the plugin keeps its sign-ins at %s, want %s", p.logins, logins)
	}
	kept, err := os.ReadFile(logins)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kept), pass) || strings.Contains(string(kept), p.token) {
		t.Fatal("the record of the sign-in holds the passphrase or the token")
	}
	if got := record(t, logins); len(got) != 1 || got["store:"+p.cli.Hello.StoreID] != "admin" {
		t.Fatalf("the record is %v, want admin for store %s", got, p.cli.Hello.StoreID)
	}
	if fi, err := os.Stat(logins); err != nil {
		t.Fatal(err)
	} else if m := fi.Mode().Perm(); m != 0o600 {
		t.Fatalf("the record's mode is %o, want 600", m)
	}
	next := New("dev")
	next.inline = true
	nh := &fakeHost{}
	next.attach(nh)
	if next.step != stepLogin || next.lastName() != "admin" {
		t.Fatalf("the next start opened at step %d with the name %q; want the sign-in, admin\n%s",
			next.step, next.lastName(), said(next))
	}
	next.Close()
	scratch := fmt.Sprintf("c%d", created) // made with conn.create alone: in no workspace
	p.useNode([]string{"w0", scratch})
	if p.step != stepReady {
		t.Fatalf("after choosing scratch: step %d\n%s", p.step, said(p))
	}

	const total = 1234
	run(p, docAt("SELECT g, md5(g::text) AS h FROM generate_series(1, "+strconv.Itoa(total)+") AS g|;\n"))
	if len(h.tables) != 1 {
		t.Fatalf("the run opened %d tables\n%s", len(h.tables), said(p))
	}
	tab := h.tables[0]
	if !strings.HasPrefix(tab.ID, "cur-") || !tab.More {
		t.Fatalf("the table is %q with more %t: want a cursor's, with rows to come", tab.ID, tab.More)
	}
	var got []string
	for _, r := range tab.Rows {
		got = append(got, r[0])
	}
	for len(h.rows) == 0 || h.rows[len(h.rows)-1].more {
		before := len(h.rows)
		p.FetchRows(plugin.TableFetch{ID: tab.ID, N: 300})
		if len(h.rows) != before+1 {
			t.Fatalf("a fetch was not answered with rows (ends %+v)\n%s", h.ends, said(p))
		}
		for _, r := range h.rows[len(h.rows)-1].rows {
			got = append(got, r[0])
		}
	}
	if len(got) != total {
		t.Fatalf("%d rows reached the table, want %d", len(got), total)
	}
	for i, v := range got {
		if v != strconv.Itoa(i+1) {
			t.Fatalf("row %d is %s, want %d: rows out of order or repeated", i, v, i+1)
		}
	}

	// the cursor ended with its last page: the session takes another statement, a new table
	run(p, docAt("SELECT 1 AS one|"))
	if len(h.tables) != 2 || h.tables[1].ID == tab.ID || h.tables[1].More {
		t.Fatalf("the second run's table: %+v\n%s", h.tables[1:], said(p))
	}

	// The tree reads the real catalog, and Enter on a table sends a SELECT built from the server's
	// own quoting, which runs.
	_, _ = rand.Read(b[:4])
	tbl := "card_tree_" + hex.EncodeToString(b[:4])
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Call(dctx, "exec.run", adminTok, created, "DROP TABLE IF EXISTS public."+tbl)
	})
	run(p, docAt(`CREATE TABLE public.`+tbl+` ("Id" integer PRIMARY KEY, "Order Total" text)|;`))
	if p.problem != "" {
		t.Fatalf("creating %s: %s", tbl, p.problem)
	}
	if p.tree == nil {
		t.Fatalf("the panel has no tree: %s", p.problem)
	}
	conn := []string{"w0", scratch}
	p.opened(conn)
	p.opened(append(conn, "s:public"))
	p.useNode(append(conn, "s:public", "t:"+tbl))
	if len(h.calls) != 1 || h.calls[0].id != "buffer.new" || h.calls[0].args[0] != "public."+tbl+".sql" {
		t.Fatalf("Enter on %s sent %q (%s)", tbl, h.calls, p.problem)
	}
	text := h.calls[0].args[1]
	if want := "SELECT\n  \"Id\",\n  \"Order Total\"\nFROM "; !strings.HasPrefix(text, want) || !strings.Contains(text, tbl) {
		t.Fatalf("the scaffold is %q, want the server-quoted columns of %s", text, tbl)
	}
	run(p, docAt("|"+text))
	if len(h.tables) != 3 || !slices.Equal(h.tables[2].Fields, []string{"Id", "Order Total"}) {
		t.Fatalf("the scaffold did not run as a SELECT of the two columns: %+v (%s)", h.tables[2:], p.problem)
	}
	p.Close()
}
