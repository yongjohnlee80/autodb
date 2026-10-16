package autodb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ReleaseBase is where AutoDB's releases are downloaded from: <base>/<version>/<asset>.
const ReleaseBase = "https://github.com/yongjohnlee80/autodb/releases/download"

// GoInstall is the command that builds AutoDB instead, for the card to name.
func GoInstall(version string) string {
	if version == "" {
		version = "latest"
	}
	return "go install github.com/yongjohnlee80/autodb/cmd/autodb@" + version
}

// releaseTag is a release's version: what the release workflow names its assets by.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// IsRelease reports whether version names a release.
func IsRelease(version string) bool { return releaseTag.MatchString(version) }

// Release is one AutoDB release's binary for one platform: the plain build, which serves without
// a window system.
type Release struct {
	Base    string // ReleaseBase, or a test's server
	Version string
	OS      string
	Arch    string
}

// ThisPlatform is version's release for the platform the plugin runs on.
func ThisPlatform(version string) Release {
	return Release{Base: ReleaseBase, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// Member is the one file the archive holds: the binary, named as the release names it.
func (r Release) Member() string {
	return fmt.Sprintf("autodb-%s-%s-%s", r.Version, r.OS, r.Arch)
}

// Asset is the archive's name.
func (r Release) Asset() string { return r.Member() + ".tar.gz" }

// URL is the archive's address; URL()+".sha256" is its checksum's.
func (r Release) URL() string {
	return strings.TrimRight(r.Base, "/") + "/" + r.Version + "/" + r.Asset()
}

// maxArchive and maxBinary bound what an install reads: an AutoDB binary is tens of megabytes.
const (
	maxArchive = 256 << 20
	maxBinary  = 512 << 20
)

// InstallError is an install that did not happen, with what the card names instead.
type InstallError struct {
	URL, GoInstall string
	Err            error
}

func (e *InstallError) Error() string {
	return fmt.Sprintf("%v (from %s); install AutoDB yourself with `%s`, or put it on PATH", e.Err, e.URL, e.GoInstall)
}

func (e *InstallError) Unwrap() error { return e.Err }

// ErrChecksum is an archive whose SHA-256 is not its .sha256's.
var ErrChecksum = errors.New("the download does not match its .sha256")

// Install downloads r's archive and its .sha256, refuses an archive whose sum differs, and unpacks
// the one binary to dest (written beside it and renamed, so dest is never a partial file).
func Install(ctx context.Context, client *http.Client, r Release, dest string) error {
	fail := func(err error) error {
		return &InstallError{URL: r.URL(), GoInstall: GoInstall(r.Version), Err: err}
	}
	if client == nil {
		client = http.DefaultClient
	}
	sumText, err := download(ctx, client, r.URL()+".sha256", 4<<10)
	if err != nil {
		return fail(fmt.Errorf("its checksum could not be read: %w", err))
	}
	want, err := parseSum(sumText, r.Asset())
	if err != nil {
		return fail(err)
	}
	archive, err := download(ctx, client, r.URL(), maxArchive)
	if err != nil {
		return fail(fmt.Errorf("the download failed: %w", err))
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fail(ErrChecksum)
	}
	if err := unpack(archive, r.Member(), dest); err != nil {
		return fail(err)
	}
	return nil
}

// parseSum reads a shasum line, "<hex>  <name>", for the asset named.
func parseSum(text []byte, asset string) (string, error) {
	fields := strings.Fields(string(text))
	if len(fields) == 0 {
		return "", errors.New("its .sha256 is empty")
	}
	sum := strings.ToLower(fields[0])
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("its .sha256 holds no SHA-256: %q", firstLine(string(text)))
	}
	if len(fields) > 1 && strings.TrimPrefix(fields[1], "*") != asset {
		return "", fmt.Errorf("its .sha256 is for %s, not %s", fields[1], asset)
	}
	return sum, nil
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return b, nil
}

// unpack writes the archive's member, a regular file, to dest with mode 0755.
func unpack(archive []byte, member, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("the archive is not gzip: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive holds no %s", member)
		}
		if err != nil {
			return fmt.Errorf("the archive is unreadable: %w", err)
		}
		if h.Name != member && h.Name != "./"+member {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("the archive's %s is not a regular file", member)
		}
		return writeBinary(io.LimitReader(tr, maxBinary+1), dest)
	}
}

func writeBinary(r io.Reader, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*.part")
	if err != nil {
		return err
	}
	part := f.Name()
	n, err := io.Copy(f, r)
	if err == nil && n > maxBinary {
		err = fmt.Errorf("the binary is larger than %d bytes", maxBinary)
	}
	if err == nil {
		err = f.Chmod(0o755)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, dest)
	}
	if err != nil {
		_ = os.Remove(part)
	}
	return err
}
