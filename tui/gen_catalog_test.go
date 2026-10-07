package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestGenerateEnglishCatalog prints autodb_en.xml to stdout when
// AUTODB_GEN_EN_CATALOG=1: the source of truth is the live catalog.
// Not a gate; a one-shot generator (ADR-0219 D5a), kept for regeneration.
func TestGenerateEnglishCatalog(t *testing.T) {
	if os.Getenv("AUTODB_GEN_EN_CATALOG") == "" {
		t.Skip("set AUTODB_GEN_EN_CATALOG=1 to regenerate qml/i18n/autodb_en.xml")
	}
	var b strings.Builder
	fmt.Fprintln(&b, "<?xml version=\"1.0\" encoding=\"utf-8\"?>")
	fmt.Fprintln(&b, "<!DOCTYPE TS>")
	fmt.Fprintln(&b, "<TS version=\"2.1\" language=\"en\">")
	fmt.Fprintln(&b, "    <context>")
	fmt.Fprintln(&b, "        <name>autodb</name>")
	seen := map[string]bool{}
	write := func(id, source string) {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		fmt.Fprintf(&b, "        <message id=%q>\n", id)
		fmt.Fprintf(&b, "            <source>%s</source>\n", esc.Replace(source))
		fmt.Fprintf(&b, "            <translation>%s</translation>\n", esc.Replace(source))
		fmt.Fprintln(&b, "        </message>")
	}
	for _, n := range menuNodes() {
		write(n.msgID(), withMnemonic(n.Label, n.Hotkey))
	}
	for _, cmd := range catalogCommands() {
		for _, mp := range cmd.Menu {
			write(mp.msgID(cmd.ID), withMnemonic(mp.Label, mp.Hotkey))
		}
		if cmd.Leader != nil {
			if cmd.Leader.Label != "" {
				write("autodb.leader."+string(cmd.ID), cmd.Leader.Label)
			}
			if cmd.Leader.LabelFor != nil {
				// The state-dependent label: both states in the catalog, the
				// ids the projection reads (ADR-0219 D5a).
				write("autodb.leader.session.connect", "connect")
				write("autodb.leader.session.disconnect", "disconnect")
			}
		}
	}
	fmt.Fprintln(&b, "    </context>")
	fmt.Fprintln(&b, "</TS>")
	os.Stdout.WriteString(b.String())
}
