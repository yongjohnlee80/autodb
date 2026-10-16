package autodb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// release serves r's archive, holding one executable member, and a .sha256 whose sum is sum
// (the archive's own when sum is "").
func release(t *testing.T, r *Release, sum string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho \"autodb " + r.Version + " (release, built then)\"\n")
	if err := tw.WriteHeader(&tar.Header{Name: r.Member(), Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	if sum == "" {
		s := sha256.Sum256(archive)
		sum = hex.EncodeToString(s[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+r.Version+"/"+r.Asset(), func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/"+r.Version+"/"+r.Asset()+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sum + "  " + r.Asset() + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r.Base = srv.URL
}

// An archive whose sum matches its .sha256 is unpacked to the binary, which answers --version.
func TestInstallUnpacksAChecksummedRelease(t *testing.T) {
	r := ThisPlatform("v1.2.3")
	release(t, &r, "")
	dest := filepath.Join(t.TempDir(), "bin", "autodb")
	if err := Install(context.Background(), nil, r, dest); err != nil {
		t.Fatalf("Install: %v", err)
	}
	v, err := Version(context.Background(), dest)
	if err != nil || v != "v1.2.3" {
		t.Fatalf("the installed binary answers %q, %v; want v1.2.3", v, err)
	}
}

// An archive whose sum is not its .sha256's is refused: nothing is written, and the error names
// the URL and the go install that builds AutoDB instead.
func TestInstallRefusesAWrongChecksum(t *testing.T) {
	r := ThisPlatform("v1.2.3")
	release(t, &r, strings.Repeat("ab", sha256.Size))
	dir := filepath.Join(t.TempDir(), "bin")
	dest := filepath.Join(dir, "autodb")
	err := Install(context.Background(), nil, r, dest)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Install with a wrong .sha256: %v, want ErrChecksum", err)
	}
	if _, serr := os.Stat(dest); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("a binary was written although its sum was wrong (%v)", serr)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("the refused install left %v", left)
	}
	for _, want := range []string{r.URL(), "go install github.com/yongjohnlee80/autodb/cmd/autodb@v1.2.3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// The assets are the release workflow's: autodb-<version>-<os>-<arch>.tar.gz and its .sha256.
func TestTheAssetIsTheReleaseWorkflows(t *testing.T) {
	r := Release{Base: ReleaseBase, Version: "v0.4.10", OS: "linux", Arch: "amd64"}
	if got, want := r.URL(), "https://github.com/yongjohnlee80/autodb/releases/download/v0.4.10/autodb-v0.4.10-linux-amd64.tar.gz"; got != want {
		t.Fatalf("URL %s, want %s", got, want)
	}
	if !IsRelease("v0.4.10") || IsRelease("dev") || IsRelease("v0.4.10-3-gabc") {
		t.Fatal("IsRelease takes vX.Y.Z only")
	}
}
