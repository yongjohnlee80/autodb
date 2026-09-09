package scriptguard

// A DECISION ABOUT PLAINTEXT TRANSPORT MUST NOT BE MADE BY SUBSTRING MATCH.
//
// dsn_is_local decides whether the installer writes
// `allow_insecure_dsn = true`, which tells autodb to accept a meta-store DSN
// with no TLS. The meta store holds the audit trail, the user records and the
// ENCRYPTED CONNECTION SECRETS, so the predicate is a security boundary.
//
// The first version matched substrings and FAILED OPEN. Review's case,
// reproduced through the shipped function before folding:
//
//	postgres://u:p@db.example/m?host=localhost.evil&sslmode=disable
//
// matched `*host=localhost*` and reported LOCAL — authorizing plaintext to a
// remote store. The function's own comment said a predicate that "guessed
// generously would be worse than the warning it replaces", and it guessed
// generously.
//
// Driven through the SHIPPED function via the define-only seam, so this is not
// a re-implementation of the rule. Both directions are asserted: the hostile
// inputs must be refused AND the legitimate local forms must still be
// accepted, because a predicate that refused everything would satisfy the
// first half and silently break every loopback install.

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// dsnIsLocal drives the real dsn_is_local from install_frontdoor.sh.
//
// AUTODB_INSTALL_DEFINE_ONLY makes the script return with every function
// defined and nothing done, so this reaches the shipped predicate without
// provisioning a host.
func dsnIsLocal(t *testing.T, dsn string) bool {
	t.Helper()
	script := fmt.Sprintf(
		`AUTODB_INSTALL_DEFINE_ONLY=1 . %q >/dev/null 2>&1
		 if dsn_is_local %q; then echo LOCAL; else echo REMOTE; fi`,
		installer(t), dsn)
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("driving dsn_is_local(%q): %v\n%s", dsn, err, out)
	}
	switch verdict := strings.TrimSpace(string(out)); verdict {
	case "LOCAL":
		return true
	case "REMOTE":
		return false
	default:
		t.Fatalf("dsn_is_local(%q) produced %q, neither verdict — the define-only seam "+
			"may no longer reach the function", dsn, verdict)
		return false
	}
}

func TestDSNLocality_HostileInputsAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, dsn string
	}{
		{
			// REVIEW'S CASE. A remote authority with a query parameter chosen
			// to satisfy a substring match.
			name: "hostile host= query on a remote authority",
			dsn:  "postgres://u:p@db.example/m?host=localhost.evil&sslmode=disable",
		},
		{
			// The precedence trap, refused rather than modelled: which of the
			// authority and the query parameter libpq honours is a question
			// this script has no business answering, so a DSN whose meaning
			// depends on it is not provably local.
			name: "remote authority with a local-looking host= parameter",
			dsn:  "postgres://u:p@db.example/m?host=localhost",
		},
		{name: "localhost as a prefix", dsn: "postgres://u:p@localhost.evil/m"},
		{name: "loopback literal as a prefix", dsn: "postgres://u:p@127.0.0.1.evil.com/m"},
		{name: "hostile keyword form", dsn: "host=localhost.evil dbname=m"},
		{
			// Short-form loopback: libpq would accept 127.1, and this refuses
			// it. Refusing costs a warning the operator can answer; accepting
			// a form the predicate cannot fully verify costs plaintext.
			name: "abbreviated loopback", dsn: "postgres://u:p@127.1/m",
		},
		{name: "not a dotted quad at all", dsn: "postgres://u:p@1270.0.0.1/m"},
		{name: "an ordinary remote host", dsn: "postgres://u:p@db.example/m"},
		{name: "an ordinary remote address", dsn: "host=10.0.0.5 dbname=m"},
		{
			// NO host named. libpq would default this to a local socket, and
			// it is still refused: the cost of refusing is a warning, and the
			// cost of guessing is plaintext to a store full of secrets.
			name: "no host at all", dsn: "postgres:///m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if dsnIsLocal(t, tc.dsn) {
				t.Errorf("dsn_is_local(%q) = LOCAL, so the installer would write "+
					"allow_insecure_dsn = true and autodb would carry the audit trail, "+
					"the user records and every encrypted connection secret to this "+
					"host in CLEARTEXT", tc.dsn)
			}
		})
	}
}

// AND THE LEGITIMATE LOCAL FORMS STILL PASS.
//
// The half that stops the fix from being "refuse everything". Without it the
// cell above is satisfied by a predicate that breaks every loopback install —
// which is the case the function exists to serve, and the one measured on
// VM43, whose PostgreSQL does not speak TLS at all.
func TestDSNLocality_TheLegitimateLocalFormsAreStillAccepted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, dsn string
	}{
		{name: "loopback with a port", dsn: "postgres://u:p@127.0.0.1:5432/m"},
		{
			// VM43's actual DSN, verbatim from the live run this branch came
			// out of. If the predicate ever stops accepting this, the run that
			// motivated the whole change stops working.
			name: "the VM43 meta store",
			dsn:  "postgres://postgres:autodbtest@127.0.0.1:55438/autodb_vm43_meta?sslmode=disable",
		},
		{name: "exact localhost", dsn: "postgres://u:p@localhost:5432/m"},
		{name: "bracketed IPv6 loopback", dsn: "postgres://u:p@[::1]:5432/m"},
		{name: "unix socket directory", dsn: "host=/var/run/postgresql dbname=m"},
		{name: "keyword loopback", dsn: "host=127.0.0.1 dbname=m"},
		{name: "keyword IPv6 loopback", dsn: "host=::1 dbname=m"},
		{
			// The whole 127/8 block is loopback, not just 127.0.0.1.
			name: "elsewhere in 127/8", dsn: "postgres://u:p@127.7.7.7:5432/m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !dsnIsLocal(t, tc.dsn) {
				t.Errorf("dsn_is_local(%q) = REMOTE, so a loopback meta store cannot be "+
					"installed without hand-editing the config the generator owns — the "+
					"defect this function was added to fix", tc.dsn)
			}
		})
	}
}
