package autodocplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
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
	h := &fakeHost{}
	p.attach(h, 70, 30)
	if p.step != stepLogin {
		t.Fatalf("the plugin did not reach the sign-in: step %d\n%s", p.step, h.card())
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
	if _, err := admin.Call(ctx, "conn.create", adminTok, "scratch", "postgres", pgurl); err != nil {
		t.Fatalf("conn.create: %v", err)
	}

	signIn(p, pass)
	if p.step != stepPick {
		t.Fatalf("after signing in: step %d\n%s", p.step, h.card())
	}
	for i, c := range p.conns {
		if c.Name == "scratch" {
			p.pick = i
		}
	}
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepReady {
		t.Fatalf("after choosing scratch: step %d\n%s", p.step, h.card())
	}

	const total = 1234
	run(p, docAt("SELECT g, md5(g::text) AS h FROM generate_series(1, "+strconv.Itoa(total)+") AS g|;\n"))
	if len(h.tables) != 1 {
		t.Fatalf("the run opened %d tables\n%s", len(h.tables), h.card())
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
			t.Fatalf("a fetch was not answered with rows (ends %+v)\n%s", h.ends, h.card())
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
		t.Fatalf("the second run's table: %+v\n%s", h.tables[1:], h.card())
	}
	p.Close()
}
