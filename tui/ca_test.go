package tui_test

import (
	"strings"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestCACertificateShowsContentsNotDaemonPathAndClearsOnClose(t *testing.T) {
	h, s := signedIn(t)
	h.FakeCA(tuiapp.CAPem{Path: "/etc/private/nonportable.pem", PEM: "-----BEGIN CERTIFICATE-----\nfixture-only\n-----END CERTIFICATE-----\n"})
	s.Keys(t, key(' '), key('k'))
	s.WaitFor(t, "CA document", func(sc string) bool {
		return strings.Contains(sc, "┌ front-door CA certificate ") && strings.Contains(sc, "fixture-only")
	})
	if strings.Contains(s.String(), "/etc/private/nonportable.pem") {
		t.Fatal("CA card disclosed a daemon-local path")
	}
	s.Keys(t, esc())
	s.WaitFor(t, "CA card closed", func(sc string) bool { return !strings.Contains(sc, "┌ front-door CA certificate ") })
	if h.SourceText("App.caText") != "" {
		t.Fatal("dismissed CA content remained in the QML source")
	}
}

func TestCACertificateExplainsSystemRootsInsteadOfShowingAnEmptyCard(t *testing.T) {
	h, s := signedIn(t)
	h.FakeCA(tuiapp.CAPem{SystemRoots: true})
	s.Keys(t, key(' '), key('k'))
	s.WaitForText(t, "no private CA")
	if strings.Contains(s.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatal("system roots were shown as a private certificate")
	}
}
