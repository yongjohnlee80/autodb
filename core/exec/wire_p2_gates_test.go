package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// PR #41 r0, lector's P2 finding: WireExecute omitted the statement-size
// invariant the token path enforces, so the two surfaces had drifted — a
// statement of any size reached the classifier here while the identical
// statement was refused on the token path.

// --------------------------------------------------------------------------
// P2 — WireExecute must reject oversized input BEFORE classification.
// --------------------------------------------------------------------------

// TestWireExecute_RejectsOversizedBeforeClassification pins the size invariant
// on the wire path.
//
// The boundary case is the half that matters. Asserting only that a huge
// statement is refused would pass against a gate placed anywhere at all,
// including one that refuses everything; asserting that cap-sized input gets
// PAST the size gate is what makes the pair discriminating.
func TestWireExecute_RejectsOversizedBeforeClassification(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)
	_ = connID

	cap := f.eng.maxStatementBytes

	// OVER the bound: refused as too large, and refused for THAT reason.
	over := "SELECT '" + strings.Repeat("x", cap) + "'"
	_, err := f.eng.WireExecute(ctx, sid, userID, over, testIP)
	if !errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("a %d-byte statement returned %v, want ErrScriptTooLarge — "+
			"the wire path classified and routed input the token path refuses", len(over), err)
	}

	// AT the bound: must not be refused for SIZE. It may fail for any other
	// reason (syntax, gate, target); what it must not be is too large.
	atCap := "SELECT '" + strings.Repeat("y", cap-len("SELECT ''")) + "'"
	if len(atCap) != cap {
		t.Fatalf("test constructed a %d-byte statement, want exactly %d", len(atCap), cap)
	}
	if _, err := f.eng.WireExecute(ctx, sid, userID, atCap, testIP); errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("a statement of exactly %d bytes was refused as too large; the bound is "+
			"inclusive on the token path and must be here too", cap)
	}
}

// TestWireExecute_OversizedControlIsRefusedToo covers the half lector named
// explicitly: the gate sits above Classify, so CONTROL statements are governed
// by it as well. A gate placed after classification, or inside the non-control
// branch, would let this through.
func TestWireExecute_OversizedControlIsRefusedToo(t *testing.T) {
	f, _, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)

	// A control verb padded past the cap by a comment. It classifies as
	// control, so it takes the wireControl route.
	over := "COMMIT -- " + strings.Repeat("z", f.eng.maxStatementBytes)
	if _, err := f.eng.WireExecute(ctx, sid, userID, over, testIP); !errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("an oversized CONTROL statement returned %v, want ErrScriptTooLarge — "+
			"the size gate is below the control routing rather than above it", err)
	}
}

// --------------------------------------------------------------------------
// P2 — LOCK takes the write floor; transaction verbs keep the read floor.
// --------------------------------------------------------------------------

// PostgreSQL permits LOCK TABLE inside a read-only transaction. Use a catalog
// that exists on every target so removing either authorization floor proves a
// reader actually takes a lock, rather than merely reaching a missing table.
const lockTarget = "pg_class"

// setRole moves the user and their grant to a role in one step, which is how
// the standing-authority cells demote and promote.
func setRole(t *testing.T, f *fixture, userID, connID int64, role string) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Users.OnCtx(ctx).With(meta.UserID, userID).
		Set(meta.UserRole, role).Update(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Grants.OnCtx(ctx).With(meta.GrantUserID, userID).
		With(meta.GrantConnID, connID).Set(meta.GrantRole, role).Update(); err != nil {
		t.Fatal(err)
	}
}

// TestWireControl_ReaderLockIsRefusedButReaderBeginIsNot binds the explicit
// wireControl floor. The positive controls prove the gate discriminates by
// verb: an editor may LOCK and a reader may still BEGIN. Removing only the
// wire gate makes the final LOCK succeed on PostgreSQL and reddens this cell.
func TestWireControl_ReaderLockIsRefusedButReaderBeginIsNot(t *testing.T) {
	f, connID, sid, _, userID := pgWireSession(t)
	ctx := context.Background()
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)

	setRole(t, f, userID, connID, meta.RoleEditor)
	if _, err := f.eng.WireExecute(ctx, sid, userID, "BEGIN", testIP); err != nil {
		t.Fatalf("editor BEGIN: %v", err)
	}
	if _, err := f.eng.WireExecute(ctx, sid, userID, "LOCK TABLE "+lockTarget, testIP); err != nil {
		t.Fatalf("an editor's LOCK did not execute: %v — an always-refuse gate would make the reader assertion meaningless", err)
	}
	_, _ = f.eng.WireExecute(ctx, sid, userID, "ROLLBACK", testIP)

	setRole(t, f, userID, connID, meta.RoleReader)
	if _, err := f.eng.WireExecute(ctx, sid, userID, "BEGIN", testIP); err != nil {
		t.Fatalf("reader BEGIN was refused: %v — lifting LOCK must not take the transaction verbs with it", err)
	}
	if _, err := f.eng.WireExecute(ctx, sid, userID, "LOCK TABLE "+lockTarget, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("reader LOCK returned %v, want auth.ErrDenied — without the wire floor a reader took a real table lock", err)
	}
}

// TestSessionExecute_ReaderLockIsRefusedOnTheTokenPathToo binds the token
// path's real floor: stateful controls re-enter run, where ClassControl maps
// explicitly to ActionDDL. A duplicate branch gate once made this cell pass
// even when that branch was deleted; collapsing the shared ClassControl floor
// to ActionRead instead makes this reader's LOCK succeed and reddens the cell.
// The same positive controls prevent an always-refuse implementation passing.
func TestSessionExecute_ReaderLockIsRefusedOnTheTokenPathToo(t *testing.T) {
	f, connID, sid, _ := pgSession(t)
	ctx := context.Background()

	root, err := f.store.Users.OnCtx(ctx).With(meta.UserName, "root").Get()
	if err != nil {
		t.Fatalf("resolving the acting user: %v", err)
	}
	userID := root.ID
	_, _ = f.eng.SessionExecute(ctx, f.rootTok, sid, "ROLLBACK", testIP)

	setRole(t, f, userID, connID, meta.RoleEditor)
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("editor BEGIN on the token path: %v", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "LOCK TABLE "+lockTarget, testIP); err != nil {
		t.Fatalf("an editor's LOCK did not execute on the token path: %v — an always-refuse floor would make the reader assertion meaningless", err)
	}
	_, _ = f.eng.SessionExecute(ctx, f.rootTok, sid, "ROLLBACK", testIP)

	setRole(t, f, userID, connID, meta.RoleReader)
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "BEGIN", testIP); err != nil {
		t.Fatalf("reader BEGIN on the token path was refused: %v — transaction verbs must keep the read floor", err)
	}
	if _, err := f.eng.SessionExecute(ctx, f.rootTok, sid, "LOCK TABLE "+lockTarget, testIP); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("reader LOCK on the token path returned %v, want auth.ErrDenied — the shared ClassControl floor did not hold", err)
	}
}
