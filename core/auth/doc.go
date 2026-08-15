// Package auth is autodb's security core (ADR-0054): identity, sessions,
// authorization, secret encryption, the IP allowlist, and the always-on
// audit trail. Every frontend (RPC, TUI, Lua, gate-guard HTTP) goes through
// this package — no security logic exists outside it (ADR-0052 §4).
//
// Key model (the keyslot envelope, ADR-0054 §1): one random 32-byte master
// key per install encrypts connection secrets; the master key is never
// stored in plaintext — each user's row carries it wrapped by a key derived
// from their passphrase (argon2id, split output: KEK + auth half). The
// Service is "locked" after process start until any user's passphrase login
// unwraps the master key into memory; secret operations before that return
// ErrLocked. Losing every passphrase loses the encrypted secrets by design;
// an admin passphrase reset (which rewraps from the unlocked key) is the
// recovery path.
package auth
