package auth

import (
	"bytes"
	"testing"
)

func TestKDF_EncodeDecodeVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	params, err := newParams()
	if err != nil {
		t.Fatal(err)
	}
	kek, authHalf := deriveKeys("correct horse", params)
	if len(kek) != keyLen || len(authHalf) != keyLen {
		t.Fatalf("split lengths = %d/%d, want %d/%d", len(kek), len(authHalf), keyLen, keyLen)
	}

	record := encodeHash(params, authHalf)
	p2, verifier, err := decodeHash(record)
	if err != nil {
		t.Fatalf("decodeHash: %v", err)
	}
	if p2.Time != params.Time || p2.Memory != params.Memory || p2.Threads != params.Threads ||
		!bytes.Equal(p2.Salt, params.Salt) {
		t.Errorf("params round-trip mismatch: %+v vs %+v", p2, params)
	}

	kek2, authHalf2 := deriveKeys("correct horse", p2)
	if !bytes.Equal(kek, kek2) {
		t.Error("KEK not deterministic for same passphrase+salt")
	}
	if !verifyAuthHalf(authHalf2, verifier) {
		t.Error("correct passphrase failed verification")
	}
	_, wrongHalf := deriveKeys("wrong horse", p2)
	if verifyAuthHalf(wrongHalf, verifier) {
		t.Error("wrong passphrase passed verification")
	}
}

// Stored params are attacker-influenced (a hostile meta-DB writer); out-of-
// bounds values must fail decode BEFORE reaching the KDF — t=0/p=0 panic
// x/crypto and huge m is memory exhaustion (lector M3 must-fix #4).
func TestDecodeHash_RejectsHostileParams(t *testing.T) {
	t.Parallel()
	params, err := newParams()
	if err != nil {
		t.Fatal(err)
	}
	_, authHalf := deriveKeys("pw", params)
	good := encodeHash(params, authHalf)

	hostile := []string{
		"argon2id$v=19$m=65536,t=0,p=4$" + good[len(good)-88:],                 // t=0 → x/crypto panic
		"argon2id$v=19$m=65536,t=1,p=0$AAAAAAAAAAAAAAAAAAAAAA$" + good[len(good)-43:], // p=0 → panic
		"argon2id$v=19$m=4294967295,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$" + good[len(good)-43:], // 4 TiB
		"argon2id$v=19$m=1024,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$" + good[len(good)-43:],       // below floor
		"argon2id$v=19$m=65536,t=999,p=4$AAAAAAAAAAAAAAAAAAAAAA$" + good[len(good)-43:],    // CPU exhaustion
		"argon2id$v=18$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$" + good[len(good)-43:],      // wrong version
		"argon2id$v=19$m=65536,t=1,p=4$AA$" + good[len(good)-43:],                          // salt too short
		"not-a-record",
	}
	for _, h := range hostile {
		if _, _, err := decodeHash(h); err == nil {
			t.Errorf("decodeHash accepted hostile record %q", h)
		}
	}
	// The genuine record still decodes.
	if _, _, err := decodeHash(good); err != nil {
		t.Errorf("decodeHash rejected the genuine record: %v", err)
	}
}

func TestSealOpen(t *testing.T) {
	t.Parallel()
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := seal(key, []byte("secret dsn"), aadSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := open(key, blob, aadSecret)
	if err != nil || string(got) != "secret dsn" {
		t.Fatalf("open = %q, %v", got, err)
	}

	// AAD binding: a blob sealed for one purpose must not open for another.
	if _, err := open(key, blob, aadMasterKey); err == nil {
		t.Error("open succeeded with mismatched AAD")
	}
	// Wrong key fails.
	other, _ := newKey()
	if _, err := open(other, blob, aadSecret); err == nil {
		t.Error("open succeeded with wrong key")
	}
	// Truncated blob fails cleanly.
	if _, err := open(key, blob[:8], aadSecret); err == nil {
		t.Error("open succeeded on truncated blob")
	}
}
