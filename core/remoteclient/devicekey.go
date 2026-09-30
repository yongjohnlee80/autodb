// Package remoteclient is the client side of a remote autodb connection that
// is not the user interface: the remote profiles a user keeps, and the device
// key that proves this machine, sealed on disk with the user's autodb
// passphrase.
package remoteclient

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// The sealing key's derivation: the daemon's approved argon2id profile for
// sign-in (t=1, 64 MiB, 4 lanes), so an offline guess at a stolen file costs
// what a guess at the server's stored hash costs.
const (
	kdfTime    = 1
	kdfMemory  = 64 * 1024 // KiB
	kdfThreads = 4
	saltLen    = 16
	keyLen     = 32
)

// fileMagic opens every sealed device-key file; the version is in it.
var fileMagic = []byte("autodb-device-key-v1\n")

var (
	// ErrNoDeviceKey: no sealed key at that path: the first connect from
	// this machine for the profile.
	ErrNoDeviceKey = errors.New("remoteclient: no device key on this machine for this profile")
	// ErrWrongPassphrase: the passphrase does not open the sealed key. On an
	// enrolled device this fails here, before anything is sent.
	ErrWrongPassphrase = errors.New("remoteclient: that passphrase does not open this device's key")
	// ErrKeyFileMode: the key file or its directory can be read by someone
	// other than its owner.
	ErrKeyFileMode = errors.New("remoteclient: the device key file is readable by others")
	// ErrKeyFileMalformed: the file is not a sealed device key.
	ErrKeyFileMalformed = errors.New("remoteclient: the device key file is not a sealed device key")
)

// aad binds a sealed key to the server and the SSH key it was enrolled for:
// the same file cannot be used against another host key or with another SSH
// key.
func aad(hostKeyFP, sshKeyFP string) []byte {
	return []byte("autodb:remote-device-key:v1:" + hostKeyFP + ":" + sshKeyFP)
}

// NewDeviceKey makes a fresh ed25519 device key.
func NewDeviceKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// Wipe overwrites a key in memory, for when the profile disconnects.
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Seal seals priv under passphrase, bound to the host key and the SSH key.
func Seal(priv ed25519.PrivateKey, passphrase, hostKeyFP, sshKeyFP string) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := sealer(passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{}, fileMagic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, priv.Seed(), aad(hostKeyFP, sshKeyFP)), nil
}

// Unseal opens a sealed key. A passphrase that does not open it, or a file
// sealed for another host key or SSH key, is ErrWrongPassphrase: the two
// cannot be told apart, and neither sends anything.
func Unseal(blob []byte, passphrase, hostKeyFP, sshKeyFP string) (ed25519.PrivateKey, error) {
	if !bytes.HasPrefix(blob, fileMagic) {
		return nil, ErrKeyFileMalformed
	}
	rest := blob[len(fileMagic):]
	if len(rest) < saltLen {
		return nil, ErrKeyFileMalformed
	}
	salt, rest := rest[:saltLen], rest[saltLen:]
	gcm, err := sealer(passphrase, salt)
	if err != nil {
		return nil, err
	}
	if len(rest) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrKeyFileMalformed
	}
	nonce, ct := rest[:gcm.NonceSize()], rest[gcm.NonceSize():]
	seed, err := gcm.Open(nil, nonce, ct, aad(hostKeyFP, sshKeyFP))
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	if len(seed) != ed25519.SeedSize {
		return nil, ErrKeyFileMalformed
	}
	defer Wipe(seed)
	return ed25519.NewKeyFromSeed(seed), nil
}

func sealer(passphrase string, salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemory, kdfThreads, keyLen)
	defer Wipe(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KeyFiles are the files of one profile's device key, in a 0700 directory:
//   - Key, the enrolled device's sealed key;
//   - Pending, a first connect's fresh key, sealed before the server is
//     asked to enroll it, and promoted to Key once it has;
//   - Next, a rotation's new key, sealed before the server swaps it in, and
//     promoted to Key once it has.
type KeyFiles struct {
	Dir, ID string
}

// DeviceKeyDir is where device keys live: $XDG_DATA_HOME/autodb/remote, or
// ~/.local/share/autodb/remote.
func DeviceKeyDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "autodb", "remote"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "autodb", "remote"), nil
}

// FilesFor is profile id's key files under dir.
func FilesFor(dir, id string) (KeyFiles, error) {
	if err := validID(id); err != nil {
		return KeyFiles{}, err
	}
	return KeyFiles{Dir: dir, ID: id}, nil
}

// Key, Pending and Next are the three files' paths.
func (k KeyFiles) Key() string     { return filepath.Join(k.Dir, k.ID+".key") }
func (k KeyFiles) Pending() string { return k.Key() + ".pending" }
func (k KeyFiles) Next() string    { return k.Key() + ".next" }

// Read reads and opens the sealed key at path (one of Key, Pending, Next).
// An absent file is ErrNoDeviceKey. The file and its directory must be
// readable by their owner only.
func (k KeyFiles) Read(path, passphrase, hostKeyFP, sshKeyFP string) (ed25519.PrivateKey, error) {
	if err := k.checkDir(); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoDeviceKey
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrKeyFileMalformed, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is mode %04o; it must be 0600", ErrKeyFileMode, path, perm)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Unseal(blob, passphrase, hostKeyFP, sshKeyFP)
}

// Exists reports whether a file is at path.
func (k KeyFiles) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Write seals priv to path, atomically and durably: a 0600 temp file in the
// directory, written, fsynced, renamed into place, and the directory fsynced.
// A crash leaves the old file or the new one, never a torn one.
func (k KeyFiles) Write(path string, priv ed25519.PrivateKey, passphrase, hostKeyFP, sshKeyFP string) error {
	blob, err := Seal(priv, passphrase, hostKeyFP, sshKeyFP)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return err
	}
	if err := k.checkDir(); err != nil {
		return err
	}
	return writeAtomic(k.Dir, "."+k.ID+".*.tmp", path, blob)
}

// Promote renames from (Pending or Next) over Key, durably.
func (k KeyFiles) Promote(from string) error {
	if err := os.Rename(from, k.Key()); err != nil {
		return err
	}
	return k.syncDir()
}

// Discard removes the file at path, if any, durably.
func (k KeyFiles) Discard(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return k.syncDir()
}

// Reseal re-seals the key at Key under newPass, having opened it with
// oldPass: the passphrase changed, the key did not.
func (k KeyFiles) Reseal(oldPass, newPass, hostKeyFP, sshKeyFP string) error {
	priv, err := k.Read(k.Key(), oldPass, hostKeyFP, sshKeyFP)
	if err != nil {
		return err
	}
	defer Wipe(priv)
	return k.Write(k.Key(), priv, newPass, hostKeyFP, sshKeyFP)
}

// RemoveAll removes the profile's key files: the profile was removed.
func (k KeyFiles) RemoveAll() error {
	for _, p := range []string{k.Key(), k.Pending(), k.Next()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return k.syncDir()
}

func (k KeyFiles) checkDir() error {
	fi, err := os.Stat(k.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: its directory %s is mode %04o; it must be 0700", ErrKeyFileMode, k.Dir, perm)
	}
	return nil
}

func (k KeyFiles) syncDir() error {
	d, err := os.Open(k.Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
