package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Amendment 8 on a live target: SET / RESET on a wire session are admitted by
// the denylist and travel RAW, so the target's own ParameterStatus reaches the
// client; refusals are the gate's, before dispatch, with no target effect.

func editorWireSession(t *testing.T) (f *fixture, sid SessionID, userID int64) {
	t.Helper()
	f, _, sid, _, userID = pgWireSession(t)
	if rb := runRaw(t, f, sid, userID, "ROLLBACK"); rb.err != nil {
		t.Fatalf("ROLLBACK the fixture's transaction: %v", rb.err)
	}
	return f, sid, userID
}

func paramStatus(frames []WireMessage, key string) (string, bool) {
	for _, m := range frames {
		if m.Kind == "ParameterStatus" && strings.EqualFold(m.ParameterName, key) {
			return m.ParameterValue, true
		}
	}
	return "", false
}

func TestWireSetReset_EditorDenylist(t *testing.T) {
	f, sid, userID := editorWireSession(t)
	ctx := context.Background()
	run := func(sql string) (frames []WireMessage, err error) {
		_, err = f.eng.WireQuery(ctx, sid, userID, sql, testIP, func(m WireMessage) error { frames = append(frames, m); return nil })
		return
	}
	// A session-level SET of a GUC that was never on the old allowlist is
	// admitted and the target reports it back.
	frames, err := run("SET datestyle TO 'German, DMY'")
	if err != nil {
		t.Fatalf("session-level SET datestyle refused on a wire session: %v", err)
	}
	if v, ok := paramStatus(frames, "DateStyle"); !ok || !strings.Contains(v, "German") {
		t.Fatalf("the target's ParameterStatus for DateStyle did not reach the client: %+v", frames)
	}
	// RESET by name is admitted and reported.
	frames, err = run("RESET datestyle")
	if err != nil {
		t.Fatalf("RESET datestyle refused: %v", err)
	}
	if v, ok := paramStatus(frames, "DateStyle"); !ok || strings.Contains(v, "German") {
		t.Fatalf("RESET did not restore DateStyle: %+v", frames)
	}
	// Editors may move search_path.
	if _, err := run("SET search_path TO public"); err != nil {
		t.Fatalf("editor SET search_path refused: %v", err)
	}
	// The denylist refuses BEFORE dispatch — no frames, the gate's error.
	for _, sql := range []string{
		"SET standard_conforming_strings TO off",
		"SET LOCAL backslash_quote TO on",
		"SET idle_in_transaction_session_timeout TO '1ms'",
		"SET ROLE postgres",
		"SET SESSION AUTHORIZATION postgres",
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"RESET ALL",
		"RESET ROLE",
	} {
		frames, err := run(sql)
		if !errors.Is(err, ErrWireSetRefused) {
			t.Fatalf("%q: want ErrWireSetRefused from the gate, got %v", sql, err)
		}
		if len(frames) != 0 {
			t.Fatalf("%q: refused statements must not reach the wire; got %d frames", sql, len(frames))
		}
	}
	if _, err := run("SET LOCAL work_mem TO '8MB'"); !errors.Is(err, ErrSetOutsideTx) {
		t.Fatalf("SET LOCAL outside a transaction: want ErrSetOutsideTx, got %v", err)
	}
}

func TestWireSetReset_ReaderDenylistAddsSearchPath(t *testing.T) {
	f, _, sid, userID, _, _ := readerWireSession(t)
	ctx := context.Background()
	run := func(sql string) error {
		_, err := f.eng.WireQuery(ctx, sid, userID, sql, testIP, func(WireMessage) error { return nil })
		return err
	}
	if err := run("SET search_path TO public"); !errors.Is(err, ErrWireSetRefused) {
		t.Fatalf("reader SET search_path must be refused, got %v", err)
	}
	if err := run("SET SCHEMA 'public'"); !errors.Is(err, ErrWireSetRefused) {
		t.Fatalf("reader SET SCHEMA (search_path alias) must be refused, got %v", err)
	}
	if err := run("RESET search_path"); !errors.Is(err, ErrWireSetRefused) {
		t.Fatalf("reader RESET search_path must be refused, got %v", err)
	}
	for _, sql := range []string{"SET default_transaction_read_only = off", "SET transaction_read_only TO off", "SET LOCAL transaction_read_only TO off"} {
		if err := run(sql); !errors.Is(err, ErrWireSetRefused) {
			t.Fatalf("%q: a reader must not lift the read-only wrap by GUC, got %v", sql, err)
		}
	}
	if err := run("SET datestyle TO 'ISO, MDY'"); err != nil {
		t.Fatalf("a reader may set an ordinary GUC: %v", err)
	}
	_ = fmt.Sprint(time.Now())
}
