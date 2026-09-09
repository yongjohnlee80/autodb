package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// config.example.toml is documentation that can go stale: it must load,
// and every value it ships uncommented must equal the default it claims
// to be. Otherwise the example teaches a configuration nobody runs.
func TestExampleConfigMatchesDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config.example.toml missing from the repo root: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	// Provenance is not configuration: `seen` records which keys the decoder
	// observed, so a loaded config has it populated and Default() cannot. The
	// claim here is about the VALUES the example documents.
	got.seen = nil
	// Load records WHERE it read from, and Default() came from nowhere, so the
	// two differ in provenance even when every configured value agrees.
	//
	// Provenance is asserted rather than merely discarded: clearing a field to
	// get a comparison to pass would also hide Load quietly stopping recording
	// it, which is the fact the store-config guard depends on.
	if got.SourcePath() != path {
		t.Errorf("Load(%q) recorded source %q", path, got.SourcePath())
	}
	got.sourcePath = ""
	got.ServiceHostSeen = false
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("example diverges from the defaults it documents:\n got %+v\nwant %+v", got, want)
	}
}

// Every settable key must APPEAR in the example (commented or not), so a
// new option cannot ship undocumented.
func TestExampleMentionsEveryKey(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, key := range tomlKeys(reflect.TypeOf(Config{})) {
		if !strings.Contains(text, key) {
			t.Errorf("config.example.toml never mentions %q", key)
		}
	}
}

// tomlKeys collects the toml tag of every field, sections included.
func tomlKeys(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		// "-" is not a key. A field tagged that way is deliberately not
		// settable from a file, and returning the literal dash made this
		// guard pass on any example containing a hyphen -- so a genuinely
		// undocumented key could ship behind a vacuous match.
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, tag)
		if f.Type.Kind() == reflect.Struct {
			out = append(out, tomlKeys(f.Type)...)
		}
	}
	return out
}

// A FIELD TAGGED toml:"-" IS NOT A KEY.
//
// tomlKeys used to hand back the literal "-", and config.example.toml is full
// of hyphens, so TestExampleMentionsEveryKey matched it unconditionally: the
// first field tagged that way satisfied the "every settable key is documented"
// guard without documenting anything, and so would every genuinely
// undocumented key added beside it.
//
// This vacuity cannot be caught by reverting the fix -- removing it makes the
// guard PASS -- so it is asserted head-on.
func TestTomlKeys_DoesNotTreatTheNotAKeyTagAsAKey(t *testing.T) {
	// The premise first: a field really is tagged that way. Without this the
	// cell below would be guarding a condition that no longer arises, and
	// would keep passing after the tag was removed.
	f, ok := reflect.TypeOf(Config{}).FieldByName("ServiceHostSeen")
	if !ok {
		t.Fatal("Config has no ServiceHostSeen field: retarget this cell at whatever " +
			"field is tagged toml:\"-\", or drop it if none is")
	}
	if got := f.Tag.Get("toml"); got != "-" {
		t.Fatalf("ServiceHostSeen is tagged %q, not \"-\"", got)
	}

	for _, k := range tomlKeys(reflect.TypeOf(Config{})) {
		if k == "-" {
			t.Error("tomlKeys yielded \"-\" as a key name: it matches any hyphen in the " +
				"example, so the documentation guard passes without documenting anything")
		}
	}
}
