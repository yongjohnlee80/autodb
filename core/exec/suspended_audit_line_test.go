package exec

// THE AUDIT MARKER, WITNESSED WITHOUT LIVE POSTGRESQL.
//
// This cell exists because of a hole in my own coverage that review found by
// mutation: it neutered suspendedSuffix to always return empty and watched
// `go test ./core/exec ./rpc ./tui -count=1` stay green.
//
// I folded the fix by asserting the audit line inside the paged-Execute cells
// — and those live in a _pg_test.go file, so they SKIP without TEST_PGURL.
// MEASURED after folding: the same mutation was STILL GREEN locally, because
// the only cells that could see it never ran. The fix had moved the hole
// rather than closed it.
//
// So the marker gets a witness on the sqlite fixture, which runs everywhere
// `go test ./...` runs. It drives writeOutcomeSuspended directly: the format
// string and the suffix function are both in the blast radius, so dropping
// either reddens this. A unit test of suspendedSuffix alone would not — the
// marker could be dropped from the Sprintf and that cell would still pass.
//
// The live paged behaviour is still covered against real PostgreSQL, where the
// suspension is genuine rather than a parameter. This is the format contract;
// that is the behaviour.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAuditLine_CarriesTheSuspensionMarker(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		suspended bool
		wantMark  bool
	}{
		{name: "a suspended page says so", suspended: true, wantMark: true},
		// The control. Without it an unconditional marker satisfies the case
		// above and the line distinguishes nothing — which is the same defect
		// as an absent marker, in the other direction.
		{name: "a completing Execute does not", suspended: false, wantMark: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()

			ident, err := f.svc.ValidateToken(ctx, f.rootTok)
			if err != nil {
				t.Fatal(err)
			}

			// histID 0: this cell is about the AUDIT line, and 0 skips the
			// history update. The history column has its own cells; asserting
			// both here would let one carry the other.
			if err := f.eng.writeOutcomeSuspended(ctx, ident, f.connID, testIP, 0,
				7*time.Millisecond, 3, StatusOK, "", "", "", tc.suspended); err != nil {
				t.Fatalf("writeOutcomeSuspended: %v", err)
			}

			rows := f.audits(t, "exec_result")
			if len(rows) != 1 {
				t.Fatalf("exec_result audit rows = %d, want exactly 1", len(rows))
			}
			detail := rows[0].Detail

			// Matched at the STATUS POSITION, not on the bare word. "suspended"
			// anywhere in the line would also be satisfied by a script, a
			// connection name or an error string containing it.
			marked := strings.Contains(detail, "(ok suspended,")
			if marked != tc.wantMark {
				t.Errorf("audit detail %q: marked=%v, want %v", detail, marked, tc.wantMark)
			}
			// And the durability token is intact either way: the marker is an
			// addition to the line, not a replacement for the status.
			if !strings.Contains(detail, "(ok") {
				t.Errorf("audit detail %q dropped the status token", detail)
			}
		})
	}
}
