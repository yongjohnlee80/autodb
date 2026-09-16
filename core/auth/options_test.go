package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// A fresh account has no preferences, and that is an empty set rather than an
// error or a nil map a caller has to guard.
func TestUserOptions_FreshAccountIsEmpty(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	opts, err := s.UserOptions(context.Background(), rootTok)
	if err != nil {
		t.Fatalf("UserOptions: %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("options = %v, want empty", opts)
	}
}

func TestSetUserOption_RoundTrips(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if err := s.SetUserOption(ctx, rootTok, OptionEditorKeyset, KeysetTextEdit, testIP); err != nil {
		t.Fatalf("SetUserOption: %v", err)
	}
	opts, err := s.UserOptions(ctx, rootTok)
	if err != nil {
		t.Fatalf("UserOptions: %v", err)
	}
	if opts[OptionEditorKeyset] != KeysetTextEdit {
		t.Fatalf("%s = %q, want %q", OptionEditorKeyset, opts[OptionEditorKeyset], KeysetTextEdit)
	}
}

// A KEY THIS BUILD HAS NEVER HEARD OF MUST SURVIVE A WRITE.
//
// This is the whole reason preferences are merged rather than replaced. A newer
// build writes a preference; this one is asked to change an unrelated one; if
// the write replaced the document, the newer build's preference would be gone
// and nothing would report it. The cell plants the unknown key directly,
// because no API in this build can write one -- which is exactly the situation
// being defended against.
func TestSetUserOption_PreservesKeysThisBuildDoesNotKnow(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, rootID := mustBootstrap(t, s)
	ctx := context.Background()

	// A document written by some future build: one key we know, one we do not.
	planted := `{"editor.keyset":"vim","colour.theme":"solarized"}`
	if err := s.store.Users.OnCtx(ctx).With(meta.UserID, rootID.UserID()).
		Set(meta.UserOptions, planted).Update(); err != nil {
		t.Fatalf("planting the document: %v", err)
	}

	if err := s.SetUserOption(ctx, rootTok, OptionEditorKeyset, KeysetTextEdit, testIP); err != nil {
		t.Fatalf("SetUserOption: %v", err)
	}

	opts, err := s.UserOptions(ctx, rootTok)
	if err != nil {
		t.Fatalf("UserOptions: %v", err)
	}
	if got := opts[OptionEditorKeyset]; got != KeysetTextEdit {
		t.Errorf("the written key = %q, want %q", got, KeysetTextEdit)
	}
	if got := opts["colour.theme"]; got != "solarized" {
		t.Errorf("the UNKNOWN key = %q, want %q — the write replaced the document "+
			"instead of merging into it", got, "solarized")
	}
}

// Two writers changing two different preferences must both survive. Sequential
// here rather than concurrent: the claim is that a write MERGES, and a race
// would test the lock instead while passing or failing on timing.
func TestSetUserOption_TwoDifferentKeysBothSurvive(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, rootID := mustBootstrap(t, s)
	ctx := context.Background()

	if err := s.store.Users.OnCtx(ctx).With(meta.UserID, rootID.UserID()).
		Set(meta.UserOptions, `{"colour.theme":"solarized"}`).Update(); err != nil {
		t.Fatalf("planting: %v", err)
	}
	if err := s.SetUserOption(ctx, rootTok, OptionEditorKeyset, KeysetVim, testIP); err != nil {
		t.Fatalf("SetUserOption: %v", err)
	}

	row, err := s.store.Users.OnCtx(ctx).With(meta.UserID, rootID.UserID()).Get()
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(row.Options), &doc); err != nil {
		t.Fatalf("the stored document is not a JSON object: %v (%q)", err, row.Options)
	}
	if len(doc) != 2 {
		t.Fatalf("document = %v, want both keys", doc)
	}
}

// An unknown KEY is refused on write, because a typo stored forever is read by
// nothing and reported by nothing.
func TestSetUserOption_RefusesAnUnknownKey(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	err := s.SetUserOption(context.Background(), rootTok, "editor.keysett", KeysetVim, testIP)
	if err == nil {
		t.Fatal("a misspelled preference key was accepted")
	}
	if !strings.Contains(err.Error(), "unknown preference") {
		t.Errorf("error = %v, want it to name the unknown preference", err)
	}
}

// A known key with a value outside its vocabulary is refused too, and the
// refusal NAMES what is accepted — an error that only says "no" leaves the
// caller guessing at the spelling.
func TestSetUserOption_RefusesAValueOutsideTheVocabulary(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)

	err := s.SetUserOption(context.Background(), rootTok, OptionEditorKeyset, "nano", testIP)
	if err == nil {
		t.Fatal("an unsupported editor profile was accepted")
	}
	for _, want := range []string{KeysetVim, KeysetTextEdit} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name the accepted value %q", err, want)
		}
	}
}
