package auth

import "errors"

var (
	// ErrLocked reports a secret operation before any passphrase login has
	// unwrapped the master key in this process (ADR-0054 §1).
	ErrLocked = errors.New("auth: store is locked — a passphrase login is required first")

	// ErrBadCredentials reports a failed login (unknown user, wrong
	// passphrase, or disabled account — deliberately indistinguishable).
	ErrBadCredentials = errors.New("auth: bad credentials")

	// ErrTokenInvalid reports an unknown, expired, or revoked session token.
	ErrTokenInvalid = errors.New("auth: invalid session token")

	// ErrDenied reports an authorization failure (missing capability, missing
	// grant, insufficient effective role, or a disallowed client IP).
	ErrDenied = errors.New("auth: permission denied")

	// ErrBootstrapDone reports a Bootstrap call on a store that already has
	// users.
	ErrBootstrapDone = errors.New("auth: store already bootstrapped")

	// ErrLastAdmin guards removing, demoting, or disabling the only enabled
	// admin — that would strand the install.
	ErrLastAdmin = errors.New("auth: refusing to remove the last enabled admin")

	// ErrWeakPassphrase reports a passphrase below the minimum length.
	ErrWeakPassphrase = errors.New("auth: passphrase must be at least 8 characters")

	// ErrKeyslotCorrupt reports a keyslot that unwrapped to a key different
	// from the process's unlocked master key.
	ErrKeyslotCorrupt = errors.New("auth: keyslot does not match the install master key")

	// ErrNoKeyslot reports an account with an empty keyslot (a pre-v2 row
	// that never received one) — an admin passphrase reset cuts a fresh
	// keyslot from the unlocked master key.
	ErrNoKeyslot = errors.New("auth: account has no master-key keyslot — an admin must reset the passphrase")
)
