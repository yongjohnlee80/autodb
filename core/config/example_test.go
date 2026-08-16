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
		if tag == "" {
			continue
		}
		out = append(out, tag)
		if f.Type.Kind() == reflect.Struct {
			out = append(out, tomlKeys(f.Type)...)
		}
	}
	return out
}
