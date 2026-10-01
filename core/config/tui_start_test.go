package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// [tui] start: local (and empty) dials this computer's daemon, ask asks, and
// remote:<profile> names a profile; anything else is refused at load, by
// --check-config too.
func TestTUIStartReadsLocalAskAndRemote(t *testing.T) {
	for _, c := range []struct {
		in     string
		remote string
		ask    bool
	}{
		{"", "", false}, {"local", "", false}, {"ask", "", true},
		{"remote:prod", "prod", false}, {" remote: prod ", "prod", false},
	} {
		remote, ask, err := TUI{Start: c.in}.StartAt()
		if err != nil || remote != c.remote || ask != c.ask {
			t.Errorf("start %q: %q %v %v; want %q %v", c.in, remote, ask, err, c.remote, c.ask)
		}
	}
	for _, bad := range []string{"remote", "remote:", "remote:two words", "somewhere", "Local"} {
		if _, _, err := (TUI{Start: bad}).StartAt(); !errors.Is(err, ErrInvalid) {
			t.Errorf("start %q: %v; want ErrInvalid", bad, err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tui]\nstart = \"elsewhere\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a config with tui.start = \"elsewhere\" loaded: %v; want ErrInvalid", err)
	}
}
