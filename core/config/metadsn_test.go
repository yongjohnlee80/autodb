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

// PR #27 r0: a DSN-level pool_max_conns must not walk past the floor.
//
// The bound used to be decided TWICE — validation read the TOML field while
// the opener treated a DSN-level pool_max_conns as authoritative — so
// `dsn = "...?pool_max_conns=1"` satisfied validation and then won at connect
// time, producing exactly the one-connection pool the floor exists to prevent
// and which the instance lease could pin whole.
func TestMetaPool_ADSNLevelBoundCannotBypassTheFloor(t *testing.T) {
	t.Parallel()

	safe := "sslmode=verify-full&sslrootcert=/c"
	insecureDSN := "postgres://x/db?" + safe + "&pool_max_conns=1"

	if _, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \""+insecureDSN+"\"\n")); err == nil {
		t.Fatal("a DSN-level pool_max_conns=1 was accepted; the floor only guards the TOML " +
			"field, so the DSN walks past it and the instance lease can pin the whole pool")
	}

	// The refusal must name WHERE the bound came from, or an operator who set
	// it in the DSN goes looking at the TOML field they never touched.
	_, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \""+insecureDSN+"\"\n"))
	if !strings.Contains(err.Error(), "dsn") {
		t.Errorf("the refusal does not say the bound came from the DSN:\n%v", err)
	}

	// A DSN bound at or above the floor is fine.
	if _, err := Load(write(t, "[meta]\nengine = \"postgres\"\ndsn = \"postgres://x/db?"+
		safe+"&pool_max_conns=4\"\n")); err != nil {
		t.Errorf("a DSN pool_max_conns=4 was refused: %v", err)
	}
}

// One function decides the effective bound, and precedence is explicit.
//
// The bug was two independent decisions about one value; this pins the single
// decision so a future caller cannot reintroduce a second one silently.
func TestMetaPool_EffectiveBoundAndItsSource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		meta   Meta
		want   int
		source string
	}{
		{"explicit TOML wins over the DSN",
			Meta{PoolMaxConns: 6, DSN: "postgres://x/db?pool_max_conns=3"}, 6, "[meta] pool_max_conns"},
		{"the DSN is honoured when TOML is unset",
			Meta{DSN: "postgres://x/db?pool_max_conns=3"}, 3, "dsn"},
		{"keyword-form DSN is read too",
			Meta{DSN: "host=h pool_max_conns=5"}, 5, "dsn"},
		{"neither set takes the default",
			Meta{DSN: "postgres://x/db"}, DefaultMetaPoolMaxConns, "default"},
	} {
		got, src := tc.meta.EffectivePoolMaxConns()
		if got != tc.want {
			t.Errorf("%s: bound = %d, want %d", tc.name, got, tc.want)
		}
		if !strings.Contains(src, tc.source) {
			t.Errorf("%s: source = %q, want it to mention %q", tc.name, src, tc.source)
		}
	}
}

// RedactDSN — lector's PR #31 r0 MF3.
//
// The migration CLI accepts both DSN forms and prints the destination in a
// report its own comment describes as "the sort of thing an operator pastes
// into a ticket". A redactor that understands only the URL form therefore
// leaks the password of every keyword-form DSN.
func TestRedactDSNMasksPasswordsInBothForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dsn  string
		keep []string // must survive: a redactor that returns "" leaks nothing and says nothing
	}{
		{"URL userinfo password",
			"postgres://autodb:sekrit@db.internal:5432/autodb?sslmode=verify-full",
			[]string{"db.internal", "autodb", "sslmode=verify-full"}},
		{"URL password query parameter",
			"postgres://db.internal/autodb?password=sekrit&sslmode=verify-full",
			[]string{"db.internal", "sslmode=verify-full"}},
		{"keyword form",
			"host=db.internal user=autodb password=sekrit dbname=autodb",
			[]string{"host=db.internal", "dbname=autodb"}},
		// libpq allows single-quoted values, so this is ONE field containing a
		// space. Splitting on whitespace masks only its first half and prints
		// the rest — which is why the redactor scans rather than uses Fields.
		{"keyword form, quoted value containing a space",
			"host=db.internal password='sekrit hunter2' dbname=autodb",
			[]string{"host=db.internal", "dbname=autodb"}},
		// libpq keywords are case-insensitive.
		{"keyword form, uppercase keyword",
			"host=db.internal PASSWORD=sekrit dbname=autodb",
			[]string{"host=db.internal", "dbname=autodb"}},
		// The password must not be confused with a value that merely contains
		// the word, nor a different key masked in its place.
		{"a non-password value is left alone",
			"host=db.internal user=password_admin dbname=autodb",
			[]string{"user=password_admin", "dbname=autodb"}},
	} {
		got := RedactDSN(tc.dsn)
		if strings.Contains(got, "sekrit") || strings.Contains(got, "hunter2") {
			t.Errorf("%s: the password survived redaction: %s", tc.name, got)
		}
		for _, keep := range tc.keep {
			if !strings.Contains(got, keep) {
				t.Errorf("%s: redaction lost %q, leaving the report unreadable: %s",
					tc.name, keep, got)
			}
		}
	}
}

// CheckOperational applies BOTH halves of the rule.
//
// It replaced a CheckDSNTransport that exposed only the transport half. The
// CLI called that half, looked validated, and let pool_max_conns=1 through to
// a destination whose lease then pinned the only connection while the
// migration runner waited for a second (MF2). An exported half is an
// invitation to apply half.
func TestCheckOperationalCoversTransportAndPoolFloor(t *testing.T) {
	t.Parallel()
	const secure = "postgres://h/db?sslmode=verify-full&sslrootcert=/ca.crt"

	if err := (Meta{DSN: secure}).CheckOperational(); err != nil {
		t.Errorf("a DSN meeting both rules was refused: %v", err)
	}
	if err := (Meta{DSN: "postgres://h/db?sslmode=require"}).CheckOperational(); err == nil ||
		!strings.Contains(err.Error(), "authenticates NOTHING") {
		t.Errorf("the transport half is not applied: %v", err)
	}
	err := (Meta{DSN: secure + "&pool_max_conns=1"}).CheckOperational()
	if err == nil {
		t.Fatal("the pool floor half is not applied: pool_max_conns=1 was accepted")
	}
	if !strings.Contains(err.Error(), "pool_max_conns in [meta] dsn") {
		t.Errorf("the refusal does not name the DSN as the source of the bound: %v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("the refusal is not an ErrInvalid: %v", err)
	}
}
