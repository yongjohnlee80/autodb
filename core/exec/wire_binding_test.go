package exec

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The auth-path half of ADR-0086: the TOKEN decides the target, and the
// startup `database` field is only asked whether it agrees.
//
// Every cell here is written against what it must OBSERVE rather than what it
// is called, because the failure this ADR's review kept finding is a cell whose
// name claims more than its assertion sees.

// setTargetDB gives a connection a target database name, which is what lets a
// client reach it by the name the TARGET knows rather than the name autodb
// gave it. Set directly: deriving it needs a real DSN, and these cells are
// about the comparison, not the derivation.
func setTargetDB(t *testing.T, f *fixture, connID int64, name string) {
	t.Helper()
	if err := f.store.Connections.OnCtx(context.Background()).With(meta.ConnID, connID).
		Set(meta.ConnTargetDB, name).Update(); err != nil {
		t.Fatalf("setting target_db: %v", err)
	}
}

// ADR-0086 cell 6. A pre-v13 token, or a v13 tombstone somebody un-revoked by
// hand, never authenticates.
//
// The DECOYS are the point. Without them the cell passes for an implementation
// that refuses the token for some entirely different reason — expiry, or the
// revoked flag it was just cleared of — and would go green while conn_id = 0
// was never consulted at all.
func TestWireBinding_TheUnscopedTombstoneNeverAuthenticates(t *testing.T) {
	t.Parallel()
	f, pat, secret, dbName := wireFixture(t)
	ctx := context.Background()

	// Sanity: this credential works BEFORE the tombstone is applied. Without
	// this the cell cannot tell "refused for being unscoped" from "this
	// fixture never worked".
	if _, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP); err != nil {
		t.Fatalf("the fixture credential must work before the tombstone: %v", err)
	}

	// conn_id = 0 AND revoked = 0: the tombstone alone must be fatal.
	if err := f.store.PATs.OnCtx(ctx).With(meta.PATID, pat.ID).
		Set(meta.PATConnID, int64(0)).Set(meta.PATRevoked, int64(0)).Update(); err != nil {
		t.Fatalf("applying the tombstone: %v", err)
	}
	_, err := f.eng.OpenWireSession(ctx, secret, "root", dbName, testIP)
	if err == nil {
		t.Fatal("an unscoped token authenticated; conn_id = 0 is a tombstone, not a value")
	}
	if got := DenialReason(err); got != DenyPATUnscoped {
		t.Errorf("reason = %q, want %q — un-revoking a v13 tombstone by hand must not bring an "+
			"unscoped credential back, and the trail must say which failure this was", got, DenyPATUnscoped)
	}
}

// ADR-0086 cell 15. What clients actually put in the `database` field, driven
// row by row rather than asserted in prose.
func TestWireBinding_TheDatabaseFieldIsAConsistencyCheck(t *testing.T) {
	t.Parallel()
	f, _, secret, connName := wireFixture(t)
	ctx := context.Background()
	setTargetDB(t, f, f.connID, "lm_prod")

	for _, tc := range []struct {
		name     string
		database string
		accepted bool
	}{
		{"the connection name", connName, true},
		// The row this whole ADR exists for: an introspecting client
		// reconnects by the database name the TARGET reported, which is not
		// the name autodb gave the connection.
		{"the target database name", "lm_prod", true},
		{"conn:<id>", "conn:" + strconv.FormatInt(f.connID, 10), true},
		{"conn:<id> zero-padded", "conn:0" + strconv.FormatInt(f.connID, 10), true},
		{"surrounding whitespace", "  " + connName + "  ", true},
		// DBeaver and pgAdmin default this field to `postgres`.
		{"a default the client filled in", "postgres", false},
		{"a different real database on the target", "other_db", false},
		{"the target name in the wrong case", "LM_PROD", false},
		{"the connection name in the wrong case", strings.ToUpper(connName), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.eng.OpenWireSession(ctx, secret, "root", tc.database, testIP)
			if tc.accepted {
				if err != nil {
					t.Fatalf("%q was refused: %v", tc.database, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q was ACCEPTED; a client that asked for one database and was quietly "+
					"given another is worse than any refusal", tc.database)
			}
			if got := DenialReason(err); got != DenyDatabaseMismatch {
				t.Errorf("reason = %q, want %q", got, DenyDatabaseMismatch)
			}
		})
	}
}

// ADR-0086 cell 1. The ambiguity this ADR removes CANNOT ARISE — and the cell
// observes WHICH connection each session actually reached.
//
// Without the routed-identity assertion this passes for an implementation
// where both tokens reach the SAME connection, which is precisely the
// local-versus-production accident the binding exists to prevent.
func TestWireBinding_TwoConnectionsNamedTheSameDatabaseCannotBeConfused(t *testing.T) {
	t.Parallel()
	f, _, secretA, _ := wireFixture(t)
	ctx := context.Background()

	// Both connections report the SAME target database name — the shape Johno
	// genuinely has: a local `test` and a production `test`.
	setTargetDB(t, f, f.connID, "test")

	bID, err := f.eng.CreateConnection(ctx, f.rootTok, "lm-prod-db", "sqlite",
		"file:wirebindb?mode=memory&cache=shared", testIP)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if uerr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, bID).
		Set(meta.ConnProfile, meta.ProfileSession).Update(); uerr != nil {
		t.Fatalf("enabling the session profile: %v", uerr)
	}
	setTargetDB(t, f, bID, "test")

	patB, err := f.svc.CreatePAT(ctx, f.rootTok, "wire-b", bID, 0, nil, false)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}

	// Both dial the string "test". They must land on DIFFERENT connections,
	// each on the one its own token names.
	gotA, err := f.eng.OpenWireSession(ctx, secretA, "root", "test", testIP)
	if err != nil {
		t.Fatalf("token A dialling \"test\": %v", err)
	}
	gotB, err := f.eng.OpenWireSession(ctx, patB.Secret, "root", "test", testIP)
	if err != nil {
		t.Fatalf("token B dialling \"test\": %v", err)
	}
	if gotA.ConnID != f.connID {
		t.Errorf("token A reached connection %d, want %d — the credential names the target, "+
			"not the string the client sent", gotA.ConnID, f.connID)
	}
	if gotB.ConnID != bID {
		t.Errorf("token B reached connection %d, want %d", gotB.ConnID, bID)
	}
	if gotA.ConnID == gotB.ConnID {
		t.Fatal("both tokens reached the SAME connection while dialling the same name — this is " +
			"the local-versus-production collision the binding exists to make impossible")
	}
}

// ADR-0086 cell 2 / §7. The grant is consulted BEFORE the `database` field, so
// an ungranted caller never produces a mismatch row — a reader of the trail
// would take that row as evidence the connection exists.
func TestWireBinding_AnUngrantedCallerAuditsNoGrantNotMismatch(t *testing.T) {
	t.Parallel()
	f, _, _, dbName := wireFixture(t)
	ctx := context.Background()

	if _, err := f.svc.CreateUser(ctx, f.rootTok, "erin", "erin-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	erinTok, erin, err := f.svc.Login(ctx, "erin", "erin-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gerr := f.svc.AddGrant(ctx, f.rootTok, erin.UserID(), f.connID, "reader", testIP); gerr != nil {
		t.Fatalf("AddGrant: %v", gerr)
	}
	erinPAT, err := f.svc.CreatePAT(ctx, erinTok, "erins", f.connID, 0, nil, false)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	if rerr := f.svc.RemoveGrant(ctx, f.rootTok, erin.UserID(), f.connID, testIP); rerr != nil {
		t.Fatalf("RemoveGrant: %v", rerr)
	}

	// A WRONG database as well as a missing grant. Both refusals are
	// available; the ordering decides which is recorded, and it must be the
	// grant — otherwise the trail says a connection exists to someone who was
	// never entitled to learn it.
	_, err = f.eng.OpenWireSession(ctx, erinPAT.Secret, "erin", "definitely-not-the-name", testIP)
	if err == nil {
		t.Fatal("an ungranted caller authenticated")
	}
	if got := DenialReason(err); got != DenyNoGrant {
		t.Errorf("reason = %q, want %q — the grant is checked BEFORE the database field, so an "+
			"ungranted caller cannot produce a mismatch row implying the connection exists",
			got, DenyNoGrant)
	}
	_ = auth.PATPrefix
	_ = dbName
}
