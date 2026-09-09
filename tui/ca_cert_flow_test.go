package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// SPC k MUST REACH THE DAEMON AND RENDER SOMETHING USEFUL.
//
// Driven through the real key rather than by calling the function, because the
// last feature in this area shipped with a mechanism that no key could reach:
// a client-side refusal rejected every input the feature existed for. A cell
// that calls the handler directly cannot catch that.
//
// The test daemon has no front door configured, so this exercises the
// system-roots branch — which is the one that must NOT be a blank float: an
// install with no private CA is a different answer from one whose file could
// not be read, and a reader has to be able to tell which they got.
func TestCACert_LeaderKReportsSystemRootsRatherThanBlank(t *testing.T) {
	h := startUI(t, startRealServer(t))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	h.leader("k")
	h.waitFor("the CA float", "front-door CA certificate")
	h.waitFor("it says there is no private CA", "no private CA")
	h.waitFor("it names the setting", "tls_root_ca_file")
	// AND it warns about the failure that misled a real operator: an unset
	// trust root makes verify-full fail with "unknown authority", and the
	// error names the certificate's host names, which reads as a name
	// mismatch and sends you the wrong way.
	h.waitFor("it warns about the misleading error", "unknown authority")
}

// THE CARD SHOWS THE CERTIFICATE AND NO PATH.
//
// This drives the branch the cells above could not reach. The harness had no
// way to configure a front door, so every CA cell exercised the system-roots
// answer -- and the PEM branch, the one an operator actually uses, had never
// been rendered by a test at all. That is how it came to carry a footnote
// naming the file on the daemon's host: the operator had asked for that path to
// be removed from the reveal card, a review caught it coming back, and nothing
// was watching.
//
// The path is worse than noise here. `SPC k` is offered to EVERY developer by
// design -- the certificate is the file you hand out -- so the footnote told
// people who cannot read that file where it lives, and it left a line of prose
// inside a document somebody selects whole and pastes into a client.
func TestCACert_ShowsTheCertificateAndNotItsPath(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "autodb-test-ca.pem")
	const pem = "-----BEGIN CERTIFICATE-----\n" +
		"MIIBtestFixtureNotARealCertificate0000000000000000000000000000000\n" +
		"-----END CERTIFICATE-----\n"
	if err := os.WriteFile(caPath, []byte(pem), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := startRealServer(t, rpc.WithFrontDoor(func() rpc.FrontDoorInfo {
		return rpc.FrontDoorInfo{Enabled: true, Listening: true, RootCAFile: caPath}
	}))
	h := startUI(t, addr)
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	h.leader("k")
	// POSITIVE CONTROL FIRST: the certificate really rendered. Without it the
	// absence assertion below is satisfied by an empty float, an error, or a
	// key that does nothing -- none of which is evidence about the path.
	h.waitFor("the CA float", "BEGIN CERTIFICATE")
	h.waitFor("the footer names the copy keys", "copy the certificate")

	screen := h.screen()
	// The path is absent by its DIRECTORY, not by the footnote's wording: an
	// assertion on "on the daemon's host" would pass the moment somebody
	// reintroduced the same disclosure under different prose.
	if strings.Contains(screen, dir) {
		t.Errorf("the CA card names the file's location on the daemon's host:\n%s", screen)
	}
	if strings.Contains(screen, "autodb-test-ca.pem") {
		t.Errorf("the CA card names the CA file:\n%s", screen)
	}
}

// AND THE ENTRY IS IN THE HELP, where somebody looks for it.
func TestCACert_LeaderMenuOffersIt(t *testing.T) {
	h := startUI(t, startRealServer(t))
	h.waitFor("about splash", "Yong Sung John Lee")
	h.key(tuicore.KeyEnter)
	h.waitFor("bootstrap float", "first run")
	h.keys("root")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyTab)
	h.keys("demo-passphrase-1")
	h.key(tuicore.KeyEnter)
	h.waitFor("login completion", "logged in as root")

	h.key(' ')
	h.waitFor("leader menu", "SPC — commands")
	h.waitFor("the CA entry", "front-door CA certificate")
	h.key(tuicore.KeyEscape)
}
