package autodb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/rpc"
)

// fakeAutodb writes an executable at path that answers --version with version, or fails when
// version is "".
func fakeAutodb(t *testing.T, path, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"autodb " + version + " (test, built now)\"\n"
	if version == "" {
		body = "#!/bin/sh\necho broken >&2\nexit 3\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// AutoDB is looked for on PATH, then in Neovim's cache, then in the plugin's own bin/, and the
// first that answers --version is taken; one that does not answer is passed over, saying why.
func TestFindTakesTheFirstPlaceThatAnswers(t *testing.T) {
	root := t.TempDir()
	pathDir := filepath.Join(root, "path")
	data := filepath.Join(root, "data")
	pluginDir := filepath.Join(root, "plugin")
	onPath := filepath.Join(pathDir, "autodb")
	cache := filepath.Join(data, "nvim", "autodb", "bin", "autodb")
	own := filepath.Join(pluginDir, "bin", "autodb")
	fakeAutodb(t, onPath, "v1.0.0-path")
	fakeAutodb(t, cache, "v2.0.0-cache")
	fakeAutodb(t, own, "v3.0.0-plugin")
	env := Env{PluginDir: pluginDir, Getenv: func(k string) string {
		return map[string]string{"PATH": pathDir, "XDG_DATA_HOME": data, "HOME": root}[k]
	}}
	ctx := context.Background()

	want := func(step, path, version, via string) {
		t.Helper()
		b, _, err := Find(ctx, env)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if b.Path != path || b.Version != version || b.Via != via {
			t.Fatalf("%s: found %+v, want %s %s (%s)", step, b, path, version, via)
		}
	}
	want("all three", onPath, "v1.0.0-path", "PATH")

	fakeAutodb(t, onPath, "") // on PATH but not answering
	b, tried, err := Find(ctx, env)
	if err != nil || b.Path != cache {
		t.Fatalf("PATH's does not answer: found %+v, %v; want the cache's", b, err)
	}
	if len(tried) != 1 || tried[0].Path != onPath || tried[0].Why == "" {
		t.Fatalf("the PATH candidate passed over is not reported with why: %+v", tried)
	}

	if err := os.Remove(onPath); err != nil {
		t.Fatal(err)
	}
	want("no PATH", cache, "v2.0.0-cache", "Neovim's AutoDB cache")
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	want("no PATH, no cache", own, "v3.0.0-plugin", "this plugin's bin/")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	_, tried, err = Find(ctx, env)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("none there: %v, want ErrNotFound", err)
	}
	if len(tried) != 3 {
		t.Fatalf("none there: tried %+v, want the three places", tried)
	}
}

// The plugin's protocol and the codes it acts on are AutoDB's own numbers.
func TestTheProtocolAndCodesAreAutoDBs(t *testing.T) {
	for _, c := range []struct {
		name       string
		mine, real int64
	}{
		{"Protocol", Protocol, rpc.Protocol},
		{"CodeProtocolMismatch", CodeProtocolMismatch, rpc.CodeProtocolMismatch},
		{"CodeAuth", CodeAuth, rpc.CodeAuth},
		{"CodeSessionNotFound", CodeSessionNotFound, rpc.CodeSessionNotFound},
		{"CodeTxState", CodeTxState, rpc.CodeTxState},
	} {
		if c.mine != c.real {
			t.Errorf("%s is %d here and %d in AutoDB", c.name, c.mine, c.real)
		}
	}
}
