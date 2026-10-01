package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// --ui starts where [tui] start says, --remote overriding it; a remote start
// names a profile that exists, or is refused before the terminal opens with
// the profiles there are.
func TestUIStartResolvesConfigAndFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.toml")
	if err := remoteclient.SaveProfiles(path, []remoteclient.Profile{
		{ID: "prod", Host: "db.example.com", User: "alice"},
		{ID: "staging", Host: "st.example.com", User: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	withStart := func(s string) config.Config {
		c := config.Default()
		c.TUI.Start = s
		return c
	}
	for _, c := range []struct {
		name, start, flag string
		want              tuiapp.Start
	}{
		{"default", "", "", tuiapp.Start{}},
		{"local", "local", "", tuiapp.Start{}},
		{"ask", "ask", "", tuiapp.Start{Ask: true}},
		{"remote in config", "remote:prod", "", tuiapp.Start{Remote: "prod"}},
		{"--remote overrides local", "local", "staging", tuiapp.Start{Remote: "staging"}},
		{"--remote overrides ask", "ask", "prod", tuiapp.Start{Remote: "prod"}},
	} {
		got, err := uiStart(withStart(c.start), c.flag, path)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v %v; want %+v", c.name, got, err, c.want)
		}
	}
	_, err := uiStart(withStart(""), "nowhere", path)
	if err == nil || !strings.Contains(err.Error(), `no remote server "nowhere"`) || !strings.Contains(err.Error(), "prod, staging") {
		t.Errorf("an unknown profile: %v; want refused, naming prod and staging", err)
	}
	empty := filepath.Join(t.TempDir(), "none.toml")
	if _, err := uiStart(withStart("remote:prod"), "", empty); err == nil || !strings.Contains(err.Error(), "Remote › Manage") {
		t.Errorf("no profiles at all: %v; want refused, saying where to add one", err)
	}
}
