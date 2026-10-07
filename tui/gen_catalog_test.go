package tui

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
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
	// The host-composed help lines (menu.go helpText).
	write("autodb.help.leader", "SPC — the leader menu:")
	write("autodb.help.search", "Pane search: / finds in the focused query or results view; n/N move to the next/previous matching row.")
	// Every qsTrId in the QML. The converted QML holds the id alone; the
	// English text is what this catalog already holds for it, so a NEW id —
	// one whose English has never been recorded — fails here rather than
	// shipping as the raw id: add it to autodb_en.xml by hand, with its
	// English, and regenerate.
	qmlIDs := map[string]bool{}
	if err := fs.WalkDir(qmlFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".qml" {
			return err
		}
		b, err := fs.ReadFile(qmlFiles, p)
		if err != nil {
			return err
		}
		for _, m := range qmlTrID.FindAllStringSubmatch(string(b), -1) {
			qmlIDs[m[1]] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	en := map[string]string{}
	if b, err := fs.ReadFile(qmlFiles, "i18n/autodb_en.xml"); err == nil {
		var ts struct {
			Messages []struct {
				ID   string `xml:"id,attr"`
				Text string `xml:"translation"`
			} `xml:"context>message"`
		}
		if err := xml.Unmarshal(b, &ts); err != nil {
			t.Fatal(err)
		}
		for _, m := range ts.Messages {
			en[m.ID] = m.Text
		}
	}
	for _, id := range slices.Sorted(maps.Keys(qmlIDs)) {
		text, ok := en[id]
		if !ok {
			t.Errorf("autodb_en.xml lacks %q — record its English there and regenerate", id)
			continue
		}
		write(id, text)
	}
	if t.Failed() {
		return
	}
	fmt.Fprintln(&b, "    </context>")
	fmt.Fprintln(&b, "</TS>")
	os.Stdout.WriteString(b.String())
}
