package autodb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BinaryName is AutoDB's executable.
const BinaryName = "autodb"

// Binary is an AutoDB executable that answered --version.
type Binary struct {
	Path    string
	Version string // the version it printed: "v0.4.10", or "dev" for a local build
	Via     string // where it was found, for the card
}

// Candidate is a place AutoDB may be, in the order they are tried.
type Candidate struct {
	Via  string
	Path string // "" when the place does not apply (nothing on PATH)
}

// Tried is a candidate and why it was not taken.
type Tried struct {
	Candidate
	Why string
}

// Env is what the search reads of the environment.
type Env struct {
	// Getenv is os.Getenv, or a test's.
	Getenv func(string) string
	// PluginDir is the plugin's own directory; its bin/autodb is the last place tried.
	PluginDir string
}

// Candidates are the places AutoDB is looked for, in order: PATH, Neovim's managed cache
// ($XDG_DATA_HOME/nvim/autodb/bin/autodb) and the plugin's own bin/autodb, where an install puts
// it.
func Candidates(env Env) []Candidate {
	get := env.Getenv
	if get == nil {
		get = os.Getenv
	}
	out := make([]Candidate, 0, 3)
	onPath := ""
	if p, err := lookPath(BinaryName, get("PATH")); err == nil {
		onPath = p
	}
	out = append(out, Candidate{Via: "PATH", Path: onPath})
	data := get("XDG_DATA_HOME")
	if data == "" {
		if home := get("HOME"); home != "" {
			data = filepath.Join(home, ".local", "share")
		}
	}
	if data != "" {
		out = append(out, Candidate{Via: "Neovim's AutoDB cache", Path: filepath.Join(data, "nvim", "autodb", "bin", BinaryName)})
	}
	if env.PluginDir != "" {
		out = append(out, Candidate{Via: "this plugin's bin/", Path: InstallPath(env.PluginDir)})
	}
	return out
}

// InstallPath is where an install puts AutoDB: the plugin's own bin/autodb.
func InstallPath(pluginDir string) string { return filepath.Join(pluginDir, "bin", BinaryName) }

// lookPath is exec.LookPath over the PATH given rather than the process's.
func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// ErrNotFound is no candidate answering --version.
var ErrNotFound = errors.New("AutoDB was not found")

// versionTimeout bounds one --version.
//
// NOTE: a deadline: a candidate that is not AutoDB (or hangs) must not hold the search, and a
// process that never exits announces nothing to wait on.
const versionTimeout = 5 * time.Second

// Find is the first candidate that answers --version as AutoDB does. When none does, it returns
// ErrNotFound with every place it tried and why each was passed over.
func Find(ctx context.Context, env Env) (Binary, []Tried, error) {
	var tried []Tried
	for _, c := range Candidates(env) {
		if c.Path == "" {
			tried = append(tried, Tried{c, "not on PATH"})
			continue
		}
		v, err := Version(ctx, c.Path)
		if err != nil {
			tried = append(tried, Tried{c, err.Error()})
			continue
		}
		return Binary{Path: c.Path, Version: v, Via: c.Via}, tried, nil
	}
	return Binary{}, tried, ErrNotFound
}

// Version runs bin --version and reads the version AutoDB prints ("autodb <version> (…)").
func Version(ctx context.Context, bin string) (string, error) {
	if _, err := os.Stat(bin); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("not there")
		}
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("--version failed: %v", err)
	}
	fields := strings.Fields(out.String())
	if len(fields) < 2 || fields[0] != BinaryName {
		return "", fmt.Errorf("--version did not answer as AutoDB does: %q", firstLine(out.String()))
	}
	return fields[1], nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
