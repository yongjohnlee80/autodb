// Command build builds the plugin as plugin.toml's [install] build runs it, with no shell:
// `go -C cmd/autodoc-autodb run ./build`, from the repository's top. It writes
// bin/autodoc-autodb at the top, stamped with the release tag of the commit it is built from (so
// the plugin installs the AutoDB release that matches it), or "dev" when the commit has none.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := build(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}
}

func build() error {
	top := os.Getenv("AUTODOC_PLUGIN_DIR") // AutoDoc sets it for the build
	if top == "" {
		top = filepath.Join("..", "..") // run from cmd/autodoc-autodb
	}
	top, err := filepath.Abs(top)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(top, "plugin.toml")); err != nil {
		return fmt.Errorf("%s is not the plugin's directory (no plugin.toml): %w", top, err)
	}
	version := releaseTag(top)
	out := filepath.Join(top, "bin", "autodoc-autodb")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags", "-X main.version="+version, "-o", out, ".")
	cmd.Dir = filepath.Join(top, "cmd", "autodoc-autodb")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Printf("built %s (%s)\n", out, version)
	return nil
}

// releaseTag is the release tag (vX.Y.Z) on the commit checked out at top, or "dev".
func releaseTag(top string) string {
	out, err := exec.Command("git", "-C", top, "tag", "--points-at", "HEAD", "--list", "v*").Output()
	if err != nil {
		return "dev"
	}
	for _, t := range strings.Fields(string(out)) {
		if isRelease(t) {
			return t
		}
	}
	return "dev"
}

// isRelease is a vX.Y.Z tag, as the release workflow names its assets.
func isRelease(t string) bool {
	parts := strings.Split(strings.TrimPrefix(t, "v"), ".")
	if !strings.HasPrefix(t, "v") || len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}
