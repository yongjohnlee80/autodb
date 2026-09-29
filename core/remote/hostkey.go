package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// ErrHostKeyMode refuses a host key file, or its directory, that another user
// could read: whoever reads the key can impersonate this server to every
// remote client.
var ErrHostKeyMode = errors.New("remote: host key is readable by other users")

// LoadOrCreateHostKey returns the remote listener's host key at path, and its
// SHA256 fingerprint, creating an ed25519 key the first time.
//
// The key is private in the same way the service keyfile is: the file must be
// 0600 or tighter and its directory 0700 or tighter, or it is refused. A new
// key's directory is created 0700 and the file written 0600 with O_EXCL, so a
// file someone placed there first is never overwritten or adopted.
func LoadOrCreateHostKey(path string) (ssh.Signer, string, error) {
	if path == "" {
		return nil, "", errors.New("remote: no host key path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("remote: host key directory: %w", err)
	}
	if err := private(dir, 0o077); err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = createHostKey(path)
	}
	if err != nil {
		return nil, "", err
	}
	if err := private(path, 0o077); err != nil {
		return nil, "", err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, "", fmt.Errorf("remote: host key %s: %w", path, err)
	}
	return signer, ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// private refuses path if any bit in mask is set in its mode.
func private(path string, mask fs.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("remote: host key: %w", err)
	}
	if info.Mode().Perm()&mask != 0 {
		return fmt.Errorf("%w: %s is %04o", ErrHostKeyMode, path, info.Mode().Perm())
	}
	return nil
}

func createHostKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("remote: generating host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "autodb remote host key")
	if err != nil {
		return nil, fmt.Errorf("remote: encoding host key: %w", err)
	}
	data := pem.EncodeToMemory(block)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("remote: writing host key: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, fmt.Errorf("remote: writing host key: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("remote: writing host key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("remote: writing host key: %w", err)
	}
	return data, nil
}
