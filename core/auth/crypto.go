package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters — the RFC 9106 low-memory recommended profile.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB (64 MiB)
	argonThreads = 4
	kdfLen       = 64 // split: 32-byte KEK + 32-byte auth half
	keyLen       = 32
	saltLen      = 16
)

// AAD labels bind each ciphertext to its purpose.
const (
	aadMasterKey = "autodb:mk:v1"
	aadSecret    = "autodb:secret:v1"
)

var b64 = base64.RawStdEncoding

// kdfParams are one user's stored derivation parameters.
type kdfParams struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	Salt    []byte
}

func newParams() (kdfParams, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return kdfParams{}, fmt.Errorf("auth: generating salt: %w", err)
	}
	return kdfParams{Time: argonTime, Memory: argonMemory, Threads: argonThreads, Salt: salt}, nil
}

// deriveKeys runs one argon2id pass and splits the output: the KEK (never
// stored) and the auth half (stored only as its SHA-256).
func deriveKeys(passphrase string, p kdfParams) (kek, authHalf []byte) {
	out := argon2.IDKey([]byte(passphrase), p.Salt, p.Time, p.Memory, p.Threads, kdfLen)
	return out[:keyLen], out[keyLen:]
}

// encodeHash renders the PHC-style pass_hash record:
// argon2id$v=19$m=<KiB>,t=<n>,p=<n>$<salt>$<sha256(authHalf)>.
func encodeHash(p kdfParams, authHalf []byte) string {
	digest := sha256.Sum256(authHalf)
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(p.Salt), b64.EncodeToString(digest[:]))
}

// Stored-parameter bounds: a hostile meta-DB writer must not be able to
// panic x/crypto (t=0, p=0) or drive CPU/memory exhaustion through absurd
// stored params (lector M3 must-fix #4). Derivation happens only inside
// these bounds.
const (
	minMemoryKiB = 8 * 1024    // 8 MiB
	maxMemoryKiB = 1024 * 1024 // 1 GiB
	maxTime      = 16
	maxThreads   = 32
	minSaltLen   = 8
	maxSaltLen   = 64
)

// decodeHash parses an encodeHash record back into params + verifier
// digest, strictly validating every stored value before it can reach the
// KDF.
func decodeHash(s string) (kdfParams, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return kdfParams{}, nil, errors.New("auth: malformed pass_hash record")
	}
	var version int
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil || version != argon2.Version {
		return kdfParams{}, nil, errors.New("auth: unsupported argon2 version in pass_hash")
	}
	var p kdfParams
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return kdfParams{}, nil, errors.New("auth: malformed argon2 params in pass_hash")
	}
	if p.Memory < minMemoryKiB || p.Memory > maxMemoryKiB {
		return kdfParams{}, nil, fmt.Errorf("auth: stored argon2 memory %d KiB outside [%d, %d]", p.Memory, minMemoryKiB, maxMemoryKiB)
	}
	if p.Time < 1 || p.Time > maxTime {
		return kdfParams{}, nil, fmt.Errorf("auth: stored argon2 time %d outside [1, %d]", p.Time, maxTime)
	}
	if p.Threads < 1 || p.Threads > maxThreads {
		return kdfParams{}, nil, fmt.Errorf("auth: stored argon2 parallelism %d outside [1, %d]", p.Threads, maxThreads)
	}
	salt, err := b64.DecodeString(parts[3])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return kdfParams{}, nil, errors.New("auth: malformed salt in pass_hash")
	}
	p.Salt = salt
	verifier, err := b64.DecodeString(parts[4])
	if err != nil || len(verifier) != 32 {
		return kdfParams{}, nil, errors.New("auth: malformed verifier in pass_hash")
	}
	return p, verifier, nil
}

// verifyAuthHalf constant-time-compares sha256(authHalf) to the stored digest.
func verifyAuthHalf(authHalf, verifier []byte) bool {
	digest := sha256.Sum256(authHalf)
	return subtle.ConstantTimeCompare(digest[:], verifier) == 1
}

// newKey returns 32 cryptographically-random bytes (the master key).
func newKey() ([]byte, error) {
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("auth: generating key: %w", err)
	}
	return k, nil
}

// seal encrypts plaintext with AES-256-GCM under key: nonce ‖ ciphertext.
func seal(key, plaintext []byte, aad string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("auth: generating nonce: %w", err)
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, []byte(aad))...), nil
}

// open decrypts a seal blob (nonce ‖ ciphertext).
func open(key, blob []byte, aad string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("auth: ciphertext too short")
	}
	return gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], []byte(aad))
}

// dummyDerive burns one KDF pass against a fixed salt so unknown-user logins
// cost the same as wrong-passphrase logins (user-enumeration timing).
func dummyDerive(passphrase string) {
	_ = argon2.IDKey([]byte(passphrase), make([]byte, saltLen), argonTime, argonMemory, argonThreads, kdfLen)
}
