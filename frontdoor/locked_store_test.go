package frontdoor

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// A LOCKED STORE answers 57P03, and it answers it POST-AUTH.
//
// THIS FILE REPLACES A SET OF CELLS THAT ENCODED A FALSE PREMISE, and the
// history is the useful part. ADR-0087 Amendment 1 A1.3 asserted that a locked
// store surfaces during the CREDENTIAL phase, from a source trace: openTarget
// decrypts the DSN and returns ErrLocked. That is true of openTarget and false
// of the credential path — OpenWireSessionWith never opens a target. Measured:
//
//	open on a LOCKED store : err=<nil>, the session OPENS
//	first query            : ErrLocked, DenialReason=""
//
// So a locked store lets a client AUTHENTICATE and refuses its first
// statement. The old cells injected ErrLocked at the credential seam and
// proved the MAPPING while the ARRIVAL never happened there — which is exactly
// the gap the reviewer named, and worse than either of us thought.
//
// The correction makes the change SMALLER: post-auth this surface already
// "answers accurately after authentication", so no pre-auth vocabulary moves
// and the R13 argument A1.3 leaned on is not needed at all.

// classifyGateError is where the answer is now decided, so this is the cell
// that pins it.
func TestLockedStore_ClassifiedAs57P03AndFatal(t *testing.T) {
	t.Parallel()
	code, rule, hint, fatal := classifyGateError(auth.ErrLocked)

	if code != LockedSQLState {
		t.Errorf("code = %q, want %q — a developer whose token is perfectly good is being "+
			"told something else", code, LockedSQLState)
	}
	if rule != string(reasonStoreLocked) {
		t.Errorf("rule = %q, want %q", rule, reasonStoreLocked)
	}
	if !fatal {
		t.Error("not fatal: the session cannot become usable without an unlock, so leaving " +
			"the connection open lets a client retry into the same refusal forever while its " +
			"pooler counts the connection as healthy")
	}
	// The HINT is where the wrong turn is refused explicitly. Post-auth this
	// surface answers accurately, so unlike the pre-auth denial there IS a
	// hint — and its whole job is to stop somebody regenerating a good token.
	for _, want := range []string{"locked", "NOT a credential", "regenerate"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint lacks %q, so the reader is not steered away from the wrong "+
				"remedy: %q", want, hint)
		}
	}
}

// THE DECOY. Every OTHER post-auth refusal keeps its own classification — the
// locked case must not have widened into a catch-all.
func TestLockedStore_OtherGateErrorsAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a lost wire face", exec.ErrWireFaceLost, sqlStateProtocolViolation},
		{"a refused wire sequence", exec.ErrWireSequenceRefused, sqlStateFeatureNotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _, _ := classifyGateError(tc.err)
			if code == LockedSQLState {
				t.Fatalf("%s was classified as a locked store; the ErrLocked case has become "+
					"a catch-all", tc.name)
			}
			if code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// AND THE CREDENTIAL PHASE IS UNCHANGED: a store failure during authentication
// still gets the uniform 28000.
//
// This is the cell that would have caught the original mistake. It asserts
// that the credential phase does NOT special-case anything for a locked store,
// which is correct precisely because a locked store never reaches it.
func TestLockedStore_CredentialPhaseStillUniform(t *testing.T) {
	t.Parallel()
	// auth.ErrLocked cannot actually arrive here — OpenWireSessionWith never
	// opens a target — but if a future change routed it here, it must not
	// quietly become a second pre-auth code without the R13 argument being
	// made again.
	f := &fakeAuth{err: auth.ErrLocked}
	_, addr := authListener(t, f)

	tc, fe := startupTo(t, addr, defaultParams())
	defer tc.Close()
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "adb_pat_aaaaaaaaaa.bbbbbbbb"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := fe.Receive()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T, want an ErrorResponse", msg)
	}
	if e.Code != DenialSQLState {
		t.Errorf("the credential phase answered %q for a store error; pre-auth stays uniform "+
			"unless the R13 argument is made again", e.Code)
	}
	if !errors.Is(auth.ErrLocked, auth.ErrLocked) {
		t.Fatal("sanity")
	}
}
