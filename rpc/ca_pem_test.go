package rpc_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

const testPEM = `-----BEGIN CERTIFICATE-----
MIIBmurmurthisIsNotARealCertificateItIsATestFixtureAAAAAAAAAAAAAAAA
-----END CERTIFICATE-----
`

// THE VERB RETURNS THE CERTIFICATE, NOT ITS PATH.
//
// The path is what the surface used to offer and it is useless to the person
// who needs the file: a developer running the TUI over a tunnel cannot read a
// file on the daemon's host, and on the host /etc/autodb/tls is 0710 -- only
// root and the service account can traverse it.
func TestCAPem_ReturnsTheCertificateContents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte(testPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo {
		return rpc.FrontDoorInfo{Enabled: true, Listening: true, RootCAFile: path}
	}))
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("frontdoor.ca_pem", f.rootTok)
	if errVal != nil {
		t.Fatalf("ca_pem: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	pem, _ := m["pem"].(string)
	if pem != testPEM {
		t.Errorf("pem = %q, want the file's contents", pem)
	}
	if got, _ := m["path"].(string); got != path {
		t.Errorf("path = %q, want %q — reported as a footnote, not instead of the file", got, path)
	}
	if sys, _ := m["system_roots"].(bool); sys {
		t.Error("a configured private CA was reported as system roots")
	}
}

// SYSTEM ROOTS IS ITS OWN ANSWER, not an empty document.
//
// An install with no private CA is a different state from one whose file could
// not be read, and a blank reply would leave a reader unable to tell which.
func TestCAPem_UnsetRootCAReportsSystemRoots(t *testing.T) {
	t.Parallel()
	f := newFixture(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo {
		return rpc.FrontDoorInfo{Enabled: true, Listening: true} // no RootCAFile
	}))
	c := f.dial(t)
	c.hello()

	errVal, result := c.call("frontdoor.ca_pem", f.rootTok)
	if errVal != nil {
		t.Fatalf("ca_pem: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	if sys, _ := m["system_roots"].(bool); !sys {
		t.Errorf("an install with no private CA did not say so: %#v", m)
	}
	if pem, _ := m["pem"].(string); pem != "" {
		t.Errorf("a system-roots install returned a document: %q", pem)
	}
}

// IT IS AUTHENTICATED, AND NOT ADMIN-ONLY.
//
// A CA certificate is public by construction -- it is what you hand out -- and
// every developer configuring a client needs it. Gating it on admin would mean
// root couriering a public file to each of them. Unauthenticated is still
// refused: what the daemon serves is not a question for a stranger.
func TestCAPem_AuthenticatedButNotAdminOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte(testPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo {
		return rpc.FrontDoorInfo{Enabled: true, Listening: true, RootCAFile: path}
	}))
	c := f.dial(t)
	c.hello()

	// An EDITOR gets it.
	errVal, _ := c.call("auth.user_create", f.rootTok, "dev", "dev-passphrase-long", "editor")
	if errVal != nil {
		t.Fatalf("user_create: %#v", errVal)
	}
	errVal, loginRes := c.call("auth.login", "dev", "dev-passphrase-long")
	if errVal != nil {
		t.Fatalf("login: %#v", errVal)
	}
	lm, _ := loginRes.(map[string]any)
	devTok, _ := lm["token"].(string)
	if devTok == "" {
		t.Fatalf("no token in the login reply: %#v", lm)
	}
	errVal, result := c.call("frontdoor.ca_pem", devTok)
	if errVal != nil {
		t.Fatalf("an editor was refused the public CA certificate: %#v", errVal)
	}
	m, _ := result.(map[string]any)
	if pem, _ := m["pem"].(string); !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Errorf("the editor's reply carries no certificate: %#v", m)
	}

	// A garbage token does not.
	errVal, _ = c.call("frontdoor.ca_pem", "adb_pat_not-a-token")
	if errVal == nil {
		t.Error("an unauthenticated caller was served the certificate")
	}
}
