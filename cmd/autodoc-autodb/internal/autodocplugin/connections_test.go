package autodocplugin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// dialogs records the dialogs the plugin opens, in order.
func dialogs(p *Plugin) *[]string {
	var got []string
	p.dialogOpened = func(id string) { got = append(got, id) }
	return &got
}

// enabled is the menu's rows that are enabled, by id.
func enabled(rows []panel.MenuRow) []string {
	var out []string
	for _, r := range rows {
		if r.Enabled {
			out = append(out, r.ID)
		}
	}
	return out
}

// A store with no user yet opens the first user's dialog, not the sign-in: a short or mismatched
// passphrase says so and asks again; made, the user is signed in, and with no workspace to see, the
// first one is offered, named AutoDoc.
func TestTheFirstRunMakesTheUserAndTheWorkspace(t *testing.T) {
	db := newFakeDB(t)
	db.noUsers, db.noWorkspaces = true, true
	p, h := testPlugin(t, db)
	opened := dialogs(p)
	p.attach(h)
	if !slices.Equal(*opened, []string{panel.DialogBootstrap}) {
		t.Fatalf("a store with no user opened %q, want the first user's dialog alone", *opened)
	}
	for _, c := range []struct{ pass, again, want string }{
		{"short", "short", "8 characters or more"},
		{"long enough", "different", "differ"},
	} {
		p.events().Bootstrap("root", c.pass, c.again)
		if !strings.Contains(news(p), c.want) || p.token != "" || len(db.took("auth.bootstrap")) != 0 {
			t.Fatalf("%q/%q: %s (signed in %v)", c.pass, c.again, said(p), p.token != "")
		}
	}
	*opened = nil
	p.events().Bootstrap("root", "a passphrase", "a passphrase")
	if p.token == "" || p.step != stepPick || len(db.took("auth.bootstrap")) != 1 {
		t.Fatalf("the first user: step %d (%s)", p.step, said(p))
	}
	if !slices.Equal(*opened, []string{panel.DialogName}) {
		t.Fatalf("with no workspace, the dialogs opened: %q", *opened)
	}
	p.events().Named("AutoDoc")
	if c := db.took("workspace.create"); len(c) != 1 || c[0].params[1] != "AutoDoc" {
		t.Fatalf("workspace.create took %+v", c)
	}
}

// The menu's rows follow the selection: a connection's on a connection, a workspace's on a
// workspace, and Add connection only into a real workspace.
func TestTheConnectionsMenuFollowsTheSelection(t *testing.T) {
	db := newFakeDB(t)
	db.loose = []map[string]any{{"id": int64(9), "name": "stray", "engine": "postgres"}}
	p, _ := connected(t, db)
	for _, c := range []struct {
		sel  []string
		want []string
	}{
		{nil, []string{"ws.new", "conn.refresh"}},
		{[]string{"w1"}, []string{"conn.add", "ws.new", "ws.rename", "ws.delete", "conn.refresh"}},
		{[]string{"w1", "c7"}, []string{"conn.add", "conn.test", "conn.rename", "conn.delete", "ws.new", "conn.refresh"}},
		{[]string{"w0"}, []string{"ws.new", "conn.refresh"}},
		{[]string{"w0", "c9"}, []string{"conn.test", "conn.rename", "conn.delete", "ws.new", "conn.refresh"}},
	} {
		p.events().Current(c.sel)
		if got := enabled(p.connectionRows()); !slices.Equal(got, c.want) {
			t.Errorf("on %q the enabled rows are %q, want %q", c.sel, got, c.want)
		}
	}
}

// Add connection writes the TLS files into the DSN as the server reads them, makes the connection
// in the workspace the dialog was opened for, and tests it; rename and delete go
// through their dialogs; Test says how long the database took.
func TestAConnectionIsAddedTestedRenamedAndDeleted(t *testing.T) {
	db := newFakeDB(t)
	p, _ := connected(t, db)
	p.events().Current([]string{"w1"})
	p.events().Menu("conn.add")
	p.events().AddConnection(panel.ConnForm{Engine: "postgres", Name: "prod", DSN: "postgres://u:p@db:5432/app",
		CA: "/etc/ssl/ca.pem", Cert: "/etc/ssl/c.pem", Key: "/etc/ssl/c.key"})
	cc := db.took("conn.create")
	if len(cc) != 1 || cc[0].params[1] != "prod" || cc[0].params[2] != "postgres" {
		t.Fatalf("conn.create took %+v", cc)
	}
	dsn := cc[0].params[3].(string)
	if !strings.Contains(dsn, "sslrootcert=%2Fetc%2Fssl%2Fca.pem") || !strings.Contains(dsn, "sslmode=verify-full") {
		t.Fatalf("the DSN %q lacks its TLS", dsn)
	}
	if a := db.took("workspace.attach"); len(a) != 1 || a[0].params[1] != int64(1) || a[0].params[2] != int64(8) {
		t.Fatalf("workspace.attach took %+v", a)
	}
	if len(db.took("conn.test")) != 1 || !strings.Contains(news(p), "prod answered") {
		t.Fatalf("the new connection was not tested: %s", said(p))
	}

	p.events().Current([]string{"w1", "c7"})
	p.events().Menu("conn.rename")
	p.events().Named("scratch2")
	if r := db.took("conn.rename"); len(r) != 1 || r[0].params[2] != "scratch2" {
		t.Fatalf("conn.rename took %+v", r)
	}
	p.events().Menu("conn.delete")
	if len(db.took("conn.delete")) != 0 {
		t.Fatal("Delete ran before its Yes")
	}
	p.events().Confirmed()
	if d := db.took("conn.delete"); len(d) != 1 || d[0].params[1] != int64(7) {
		t.Fatalf("conn.delete took %+v", d)
	}
}

// tlsFiles writes a self-signed certificate (its own CA) and its key under a temporary directory:
// the server's parser reads the files a DSN names.
func tlsFiles(t *testing.T) (ca, cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "db"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ca, cert, key = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "c.pem"), filepath.Join(dir, "c.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for path, b := range map[string][]byte{ca: certPEM, cert: certPEM,
		key: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ca, cert, key
}

// tlsDSN writes each engine's TLS files as the AutoDB server reads them: its own ValidateDSN
// takes the result (reading the files, as pgx does), a certificate needs its key, and SQLite has
// none.
func TestTLSFilesAreWrittenAsTheServerReadsThem(t *testing.T) {
	ca, cert, key := tlsFiles(t)
	q := url.QueryEscape
	for _, c := range []struct {
		engine, dsn string
		want        []string
	}{
		{"postgres", "postgres://u:p@db:5432/app", []string{"sslrootcert=" + q(ca), "sslcert=" + q(cert), "sslkey=" + q(key), "sslmode=verify-full"}},
		{"postgres", "postgres://u:p@db:5432/app?sslmode=require", []string{"sslmode=require&sslrootcert"}},
		{"postgres", "host=db user=u dbname=app", []string{"sslrootcert='" + ca + "'", "sslmode='verify-full'"}},
		{"mysql", "u:p@tcp(db:3306)/app", []string{"?tls-ca=" + q(ca) + "&tls-cert=" + q(cert) + "&tls-key=" + q(key)}},
		{"mysql", "u:p@tcp(db:3306)/app?parseTime=true", []string{"&tls-ca=" + q(ca)}},
	} {
		got, err := tlsDSN(c.engine, c.dsn, ca, cert, key)
		if err != nil {
			t.Fatalf("%s: %v", c.dsn, err)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s gave %q, wanting %q in it", c.dsn, got, w)
			}
		}
		name, _ := engine.Parse(c.engine)
		if err := exec.ValidateDSN(name, got); err != nil {
			t.Errorf("the server refuses %q: %v", got, err)
		}
	}
	if _, err := tlsDSN("postgres", "postgres://db/app", "", "/c.pem", ""); err == nil {
		t.Error("a certificate without its key was taken")
	}
	if _, err := tlsDSN("sqlite", "/tmp/x.db", "/ca.pem", "", ""); err == nil {
		t.Error("SQLite took TLS files")
	}
	if got, _ := tlsDSN("sqlite", "/tmp/x.db", "", "", ""); got != "/tmp/x.db" {
		t.Errorf("a DSN with no TLS files changed: %q", got)
	}
}
