package rpc_test

// THE SUSPENSION AXIS HAS TO REACH THE OPERATOR, AND IT STOPPED AT THE DAO.
//
// Review found this and I reproduced it before folding. The stored column was
// written correctly and read correctly by core/meta, and then nothing carried
// it any further: core/exec.HistoryRow had no field, history.list projected no
// key, and tui/client could not decode one. So the surface an operator
// actually reads rendered a suspended Execute and a completed one identically
// as status=ok — the very confusion the new axis exists to remove, still fully
// intact everywhere except inside the DAO.
//
// Driven THROUGH THE DISPATCH, per this fixture's own rule: a cell that called
// ListHistory directly would stay green if the verb's projection dropped the
// key, which is exactly the defect being fixed. The row is seeded straight into
// the store because nothing autodb ships sends a row limit — no in-process
// driver can produce a suspended Execute, so the wire projection has to be
// tested against a store state rather than against a live suspension. The live
// suspension is covered on real PostgreSQL in core/exec.

import (
	"context"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

func TestHistoryList_ProjectsTheSuspensionAxis(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Two rows differing ONLY in the suspension flag, with the same status.
	// That pairing is the whole measurement: it proves the key tracks the
	// column rather than being derived from status, and it would fail a
	// projection that hard-coded either value.
	seed := func(script string, suspended int64) {
		t.Helper()
		if _, err := f.store.History.OnCtx(ctx).
			Set(meta.HistUserID, int64(1)).Set(meta.HistConnID, f.connID).
			Set(meta.HistIP, "127.0.0.1").Set(meta.HistScript, script).
			Set(meta.HistStartedAt, int64(1788900000)).
			Set(meta.HistDurationMS, int64(7)).Set(meta.HistRowCount, int64(3)).
			Set(meta.HistStatus, "ok").Set(meta.HistError, "").
			Set(meta.HistTxID, "").
			Set(meta.HistSuspended, suspended).
			Insert(); err != nil {
			t.Fatalf("seeding %q: %v", script, err)
		}
	}
	seed("SELECT 'a-suspended-page'", 1)
	seed("SELECT 'a-completed-statement'", 0)

	c := f.session(t)
	errVal, result := c.call("history.list", f.rootTok, int64(50))
	if errVal != nil {
		t.Fatalf("history.list: %v", errVal)
	}

	rows, ok := result.([]any)
	if !ok {
		t.Fatalf("result is %T, want a list", result)
	}
	got := map[string]any{}
	for _, r := range rows {
		m, isMap := r.(map[string]any)
		if !isMap {
			t.Fatalf("row is %T, want a map", r)
		}
		script, _ := m["script"].(string)
		switch script {
		case "SELECT 'a-suspended-page'", "SELECT 'a-completed-statement'":
			// PREMISE: the key is PRESENT on the wire. A missing key decodes
			// to false in the client, which is indistinguishable from a
			// truthful "not suspended" — so its absence has to be caught
			// here, where the raw map can still be inspected, rather than
			// downstream where it is silently a zero value.
			v, present := m["suspended"]
			if !present {
				t.Fatalf("history.list emitted no \"suspended\" key for %s. A client "+
					"reading this decodes false, which is the same answer it gave "+
					"before the axis existed — the defect is invisible downstream",
					script)
			}
			got[script] = v
			// AND status is untouched: the durability token still says the
			// effects committed, which is true of both rows.
			if s, _ := m["status"].(string); s != "ok" {
				t.Errorf("%s projected status %q, want \"ok\" — suspension must not "+
					"have re-scoped the effect token", script, s)
			}
		}
	}

	if len(got) != 2 {
		t.Fatalf("found %d of the 2 seeded rows in the projection: %v", len(got), got)
	}
	if v := got["SELECT 'a-suspended-page'"]; v != true {
		t.Errorf("the suspended row projected suspended=%v (%T), want true", v, v)
	}
	if v := got["SELECT 'a-completed-statement'"]; v != false {
		t.Errorf("the completed row projected suspended=%v (%T), want false — a "+
			"projection that always reported true would satisfy the assertion above "+
			"and distinguish nothing", v, v)
	}
}
