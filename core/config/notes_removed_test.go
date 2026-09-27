package config_test

// The identity-keying change removed web.notes_mode / web.notes_subject. What must be tested is
// not the absence of a feature but that a config still carrying either key
// SAYS SO, with a reason.
//
// Being ignored SILENTLY is the dangerous outcome. An operator who set
// notes_mode did so for isolation, and dropping it without a word leaves them
// believing an isolation setting is in force when it no longer exists. An
// unknown key no longer refuses the load — a retired key must not stop the
// daemon or an update (docs/ops/schema-scripts.md) — so the reason is a
// warning, and this holds that it is there, naming the removal and what
// replaced it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "autodb.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemovedNotesKeysWarnWithAReason(t *testing.T) {
	for _, tc := range []struct{ name, body, key string }{
		{"notes_mode", "[web]\nnotes_mode = \"workspace\"\n", "web.notes_mode"},
		{"notes_subject", "[web]\nnotes_subject = \"root\"\n", "web.notes_subject"},
		{"both", "[web]\nnotes_mode = \"workspace\"\nnotes_subject = \"root\"\n", "web.notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("a retired key refused the load: %v", err)
			}
			msg := strings.Join(cfg.Warnings(), "\n")
			if !strings.Contains(msg, tc.key) {
				t.Fatalf("no warning names %s: %q — ignoring it silently leaves the operator "+
					"believing the setting still applies", tc.key, msg)
			}
			if !strings.Contains(msg, "identity-keyed") {
				t.Errorf("error does not name the removal: %q", msg)
			}
			if !strings.Contains(msg, "user, workspace") {
				t.Errorf("error does not say what replaced it: %q", msg)
			}
		})
	}
}

// The paired positive: a config WITHOUT those keys still loads. Without it,
// rejecting every [web] table would satisfy the test above.
func TestConfigWithoutRemovedKeysStillLoads(t *testing.T) {
	if _, err := config.Load(writeConfig(t, "[meta]\nengine = \"sqlite\"\n")); err != nil {
		t.Fatalf("a config with no notes keys must still load: %v", err)
	}
}
