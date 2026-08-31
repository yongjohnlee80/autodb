package config

import (
	"errors"
	"strings"
	"testing"
)

// Meta-store transport hardening — ADR-0079 §4 / P1.
//
// The meta store holds the audit trail, the user records and the encrypted
// connection secrets, so its DSN is checked at load rather than trusted.

// Every mode weaker than verify-full is refused, and the refusal says WHY —
// an operator who set `require` believes they have TLS, and "invalid dsn"
// would not correct that belief.
func TestMetaDSN_RefusesEveryModeThatCannotAuthenticateTheServer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode    string
		mustSay string
	}{
		{"disable", "plaintext"},
		{"allow", "plaintext"},
		{"prefer", "plaintext"},
		{"require", "authenticates NOTHING"},
		{"verify-ca", "belongs to the host"},
	} {
		dsn := "postgres://u@h/db?sslmode=" + tc.mode
		err := checkMetaDSNTransport(dsn, false)
		if err == nil {
			t.Errorf("sslmode=%s was accepted for the meta store", tc.mode)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("sslmode=%s: err = %v, want ErrInvalid", tc.mode, err)
		}
		if !strings.Contains(err.Error(), tc.mustSay) {
			t.Errorf("sslmode=%s: the refusal does not explain the risk (%q missing):\n%v",
				tc.mode, tc.mustSay, err)
		}
	}
}

// An ABSENT sslmode is `prefer`, not "unspecified".
//
// This is the one an operator gets wrong by omission rather than by choice:
// libpq silently defaults to prefer, which falls back to plaintext. Treating
// absence as acceptable would leave the commonest insecure DSN unguarded.
func TestMetaDSN_AnAbsentSSLModeIsTreatedAsPrefer(t *testing.T) {
	t.Parallel()

	err := checkMetaDSNTransport("postgres://u@h/db", false)
	if err == nil {
		t.Fatal("a DSN with no sslmode was accepted; libpq defaults to prefer, which falls " +
			"back to plaintext")
	}
	if !strings.Contains(err.Error(), "prefer") {
		t.Errorf("the refusal does not name the effective mode:\n%v", err)
	}
}

// verify-full without a root certificate is refused too.
//
// It would otherwise verify against ~/.postgresql/root.crt if that happens to
// exist on whichever host runs the daemon — which makes the security property
// depend on the filesystem rather than on the configuration.
func TestMetaDSN_VerifyFullStillNeedsAnExplicitRootCert(t *testing.T) {
	t.Parallel()

	if err := checkMetaDSNTransport("postgres://u@h/db?sslmode=verify-full", false); err == nil {
		t.Fatal("verify-full with no sslrootcert was accepted")
	}
	if err := checkMetaDSNTransport(
		"postgres://u@h/db?sslmode=verify-full&sslrootcert=/etc/ssl/ca.crt", false); err != nil {
		t.Fatalf("a fully-specified DSN was refused: %v", err)
	}
}

// The check must understand BOTH DSN forms, or it is bypassed by reformatting.
func TestMetaDSN_KeywordFormIsCheckedToo(t *testing.T) {
	t.Parallel()

	if err := checkMetaDSNTransport("host=db.internal user=autodb sslmode=require", false); err == nil {
		t.Fatal("a keyword-form DSN with sslmode=require was accepted; the check reads only " +
			"URLs, so it can be walked around by rewriting the DSN")
	}
	if err := checkMetaDSNTransport(
		"host=db.internal user=autodb sslmode=verify-full sslrootcert=/etc/ssl/ca.crt", false); err != nil {
		t.Fatalf("a safe keyword-form DSN was refused: %v", err)
	}
}

// The opt-out works, and is the ONLY way past the check.
func TestMetaDSN_TheOptOutIsExplicitAndSufficient(t *testing.T) {
	t.Parallel()

	if err := checkMetaDSNTransport("postgres://u@h/db?sslmode=disable", true); err != nil {
		t.Fatalf("allow_insecure_dsn did not permit an insecure DSN: %v", err)
	}
	// And it is genuinely needed — the same DSN without it is refused.
	if err := checkMetaDSNTransport("postgres://u@h/db?sslmode=disable", false); err == nil {
		t.Fatal("the insecure DSN was accepted WITHOUT the opt-out, so the opt-out proves nothing")
	}
}

// The pool floor: below two, anything that pins a connection starves the work
// beside it — which is exactly the deadlock the migration lock hit.
func TestConfig_MetaPoolFloor(t *testing.T) {
	t.Parallel()

	base := "[meta]\nengine = \"postgres\"\ndsn = \"postgres://x/db?sslmode=verify-full&sslrootcert=/c\"\n"
	if _, err := Load(write(t, base+"pool_max_conns = 1\n")); err == nil {
		t.Error("pool_max_conns = 1 was accepted; the instance lease pins one connection for " +
			"the daemon's lifetime, so nothing else could run")
	}
	if _, err := Load(write(t, base+"pool_max_conns = -1\n")); err == nil {
		t.Error("a negative pool_max_conns was accepted")
	}
	// 0 means "take the default", and 2 is the floor.
	for _, ok := range []string{"pool_max_conns = 0\n", "pool_max_conns = 2\n"} {
		if _, err := Load(write(t, base+ok)); err != nil {
			t.Errorf("%q was refused: %v", strings.TrimSpace(ok), err)
		}
	}
}

// The check must be REACHED from Load, not merely exist.
//
// Every cell above calls checkMetaDSNTransport directly, and all of them stay
// green if the call is deleted from Validate — which would leave the function
// correct, tested, and never run. That is code-review §7: a test that reaches
// the state by hand proves the constructor, not the system.
func TestLoad_RefusesAnInsecureMetaDSNThroughTheRealEntryPoint(t *testing.T) {
	t.Parallel()

	_, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \"postgres://x/db?sslmode=require\"\n"))
	if err == nil {
		t.Fatal("Load accepted a meta DSN with sslmode=require; the transport check exists but " +
			"is not wired into validation, so it never runs on a real config")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "authenticates NOTHING") {
		t.Errorf("the refusal reached Load but lost its explanation:\n%v", err)
	}

	// And the opt-out reaches Load too, or an operator could not deploy on a
	// trusted local channel at all.
	if _, err := Load(write(t, "[meta]\nengine = \"postgres\"\n"+
		"dsn = \"postgres://x/db?sslmode=disable\"\nallow_insecure_dsn = true\n")); err != nil {
		t.Errorf("allow_insecure_dsn is not honoured through Load: %v", err)
	}

	// sqlite is unaffected — the check must not fire on an engine with no DSN.
	if _, err := Load(write(t, "[meta]\nengine = \"sqlite\"\n")); err != nil {
		t.Errorf("a sqlite config was refused by the postgres transport check: %v", err)
	}
}
