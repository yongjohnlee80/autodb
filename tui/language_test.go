// language_test.go — the localization gates: the three catalog checks (D7's
// inventory and coverage, and golib's CheckLanguages walk), the live switch
// with a dialog open, the mnemonic baseline after the label conversion, and
// the stored-language fallback a build that dropped a language needs.
package tui_test

import (
	"encoding/xml"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

// parseCatalog reads one TS XML catalog: id → translation.
func parseCatalog(t *testing.T, fsys fs.FS, name string) map[string]string {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var ts struct {
		Messages []struct {
			ID          string `xml:"id,attr"`
			Translation string `xml:"translation"`
		} `xml:"context>message"`
	}
	if err := xml.Unmarshal(b, &ts); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string]string{}
	for _, m := range ts.Messages {
		out[m.ID] = m.Translation
	}
	return out
}

// inventory is every id the program can show: every projected bar row, every
// leader label, and every qsTrId the QML holds.
func inventory(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, id := range tuiapp.CatalogInventoryForTest() {
		ids[id] = true
	}
	for _, id := range tuiapp.QMLMessageIDsForTest() {
		ids[id] = true
	}
	return ids
}

// TestTheEnglishCatalogHoldsEveryID — the inventory check: every id the
// program can show exists in autodb_en.xml, or a screen would show the raw id.
func TestTheEnglishCatalogHoldsEveryID(t *testing.T) {
	en := parseCatalog(t, tuiapp.CatalogFiles(), "i18n/autodb_en.xml")
	for id := range inventory(t) {
		if _, ok := en[id]; !ok {
			t.Errorf("autodb_en.xml lacks %q — the screen would show the raw id", id)
		}
	}
}

// TestEveryShippedLanguageCoversEveryID — the coverage check: each shipped
// translation holds every inventoried id. A gap is a failure here, not a
// silent English row on a translated screen.
func TestEveryShippedLanguageCoversEveryID(t *testing.T) {
	inv := inventory(t)
	for _, tag := range auth.Languages {
		if tag == "en" {
			continue
		}
		cat := parseCatalog(t, tuiapp.CatalogFiles(), "i18n/autodb_"+tag+".xml")
		var missing []string
		for id := range inv {
			if _, ok := cat[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s lacks %d ids: %s", tag, len(missing), strings.Join(missing, ", "))
		}
	}
}

// TestTheCatalogsMatchTheShippedLanguages: one file per language, exactly.
func TestTheCatalogsMatchTheShippedLanguages(t *testing.T) {
	entries, err := fs.ReadDir(tuiapp.CatalogFiles(), "i18n")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, e := range entries {
		if path.Ext(e.Name()) == ".xml" {
			files[strings.TrimSuffix(strings.TrimPrefix(e.Name(), "autodb_"), ".xml")] = true
		}
	}
	for _, tag := range auth.Languages {
		if !files[tag] {
			t.Errorf("no catalog for %q", tag)
		}
		delete(files, tag)
	}
	for tag := range files {
		t.Errorf("catalog %q is not a language the product offers", tag)
	}
}

// TestEveryLanguageWalksTheScreen — golib's own check over the mounted tree:
// mnemonics, collisions, and the run-time English-letter rule, per language.
func TestEveryLanguageWalksTheScreen(t *testing.T) {
	decltest.CheckLanguages(t, tuiapp.ProgramOptions(tuiapp.Options{})...)
}

// TestTheMenuMnemonicSurvivesTheMessageConversion — the dynamic menu walk:
// every row kind (top bar, submenu, command) keeps its hotkey marker and its
// access key in English, and the same rows in Korean mark English's letter
// (golib's run-time rule: the English letter, in every language).
func TestTheMenuMnemonicSurvivesTheMessageConversion(t *testing.T) {
	escKey := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEscape}

	for _, tc := range []struct{ name, tag string }{
		{"English", "en"}, {"Korean", "ko_KR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := tuiapp.RunHost(t, tuiapp.NewSession("127.0.0.1:1", logger.Nop{}, nil), nil,
				tuiapp.Options{App: []tuicore.AppOption{tuicore.WithLanguage(tc.tag)}}, 100, 32)
			// Top bar: Alt+F opens File, in every language.
			s.Keys(t, decltest.Alt('f'))
			s.WaitFor(t, "File open on Alt+F", func(sc string) bool { return strings.Contains(sc, "New note") || strings.Contains(sc, "새 노트") })
			// A command row inside it: Alt+N opens New note.
			s.Keys(t, decltest.Alt('n'))
			s.WaitFor(t, "New note's dialog", func(sc string) bool { return strings.Contains(sc, "note") || strings.Contains(sc, "노트") })
			s.Keys(t, escKey)
			// A submenu: Options opens; its Theme child is a submenu row —
			// navigate with arrows and Enter, and the themes show.
			s.Keys(t, decltest.Alt('o'))
			s.WaitFor(t, "Options open", func(sc string) bool { return strings.Contains(sc, "Editor") || strings.Contains(sc, "편집기") })
			down := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyDown}
			enter := tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEnter}
			s.Keys(t, down, enter)
			s.WaitFor(t, "Theme submenu open", func(sc string) bool {
				return strings.Contains(sc, "Dark") || strings.Contains(sc, "다크")
			})
			s.Keys(t, escKey, escKey, escKey)
		})
	}
}

// TestSwitchingLanguageRelabelsTheOpenScreen — the live switch: Options ›
// Language › 한국어 relabels the menu bar in that frame; the check moves in
// the same turn; back to English restores it. The quit dialog is open across
// the switch and stays open after it (D7).
func TestSwitchingLanguageRelabelsTheOpenScreen(t *testing.T) {
	h, s := tuiapp.RunHost(t, tuiapp.NewSession("127.0.0.1:1", logger.Nop{}, nil), nil,
		tuiapp.Options{}, 100, 32)
	s.WaitForText(t, "Home")

	// Open a dialog, then switch with it open: the language source moves in
	// the same turn, and the dialog stays open across the switch. The dim
	// backdrop hides the bar from the painted screen, so the switch is read
	// as the App's own answer: the source value the check binds.
	s.Keys(t, decltest.Ctrl('q'))
	s.WaitForText(t, "quit autodb?")
	h.RunCommand("options.language.ko_KR")
	s.WaitFor(t, "the language source moved", func(string) bool {
		return h.SourceText("App.language") == "ko_KR"
	})
	if !h.SourceBool("App.lang_ko_KR") {
		t.Fatal("the language check did not move in the same turn")
	}

	// Back to English: the dialog is still there, and the source restores.
	h.RunCommand("options.language.en")
	s.WaitFor(t, "back to English", func(sc string) bool {
		return h.SourceText("App.language") == "en" && strings.Contains(sc, "quit autodb?")
	})
	if !strings.Contains(s.String(), "quit autodb?") {
		t.Fatal("the dialog did not survive the switches")
	}

	// The bar relabelled underneath the backdrop: Esc the dialog away and
	// read it painted.
	s.Keys(t, tuicore.KeyEvent{Kind: tuicore.KeyPress, Code: tuicore.KeyEscape})
	s.WaitFor(t, "the bar in Korean then English", func(sc string) bool {
		return strings.Contains(sc, "Home") && strings.Contains(sc, "Options")
	})
}

// TestAStoredLanguageTheBuildDoesNotOfferFallsBackToEnglish — the downgrade
// case: a stored option naming a language this build dropped never reaches
// the App; the screen comes up English and the stored value stays as it was
// (a stored value a build does not offer never reaches the App).
func TestAStoredLanguageTheBuildDoesNotOfferFallsBackToEnglish(t *testing.T) {
	if got := tuiapp.LanguageOfForTest("fr"); got != "en" {
		t.Fatalf("an unoffered tag resolves to %q, want en", got)
	}
	if got := tuiapp.LanguageOfForTest("ko_KR"); got != "ko_KR" {
		t.Fatalf("an offered tag resolves to %q, want ko_KR", got)
	}
}
