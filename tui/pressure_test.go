package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
)

func TestPressureView_AnUnreadableViewNamesItselfInsteadOfLookingCalm(t *testing.T) {
	p := newPressureView()
	p.apply(&Host{}, pressure.Snapshot{}, errors.New("no pressure source is wired"), time.Now())
	if p.model.Len() != 1 || p.model.At(0)["measure"] != "unavailable" ||
		!strings.Contains(p.model.At(0)["value"].(string), "no pressure source") {
		t.Fatalf("an unreadable view looked calm: %#v", p.model.At(0))
	}
	good := pressure.Snapshot{Sessions: pressure.Row{Value: 3, Cap: 10}}
	landed := time.Now().Add(-10 * time.Second)
	p.apply(&Host{}, good, nil, landed)
	p.apply(&Host{}, pressure.Snapshot{}, errors.New("refresh failed"), time.Now())
	if !p.have || p.model.At(1)["value"] != "3 / 10" || p.failure != "refresh failed" {
		t.Fatalf("the failed refresh hid the last good figures instead of marking them stale: %#v", p.model.At(1))
	}
}

func TestPressureView_EveryRefusalShowsWhetherItIsUsOrThem(t *testing.T) {
	s := pressure.Snapshot{Denials: []pressure.DenialRow{
		{DenialKey: pressure.DenialKey{Reason: "pool-full", Class: pressure.Capacity}, Count: 4},
		{DenialKey: pressure.DenialKey{Reason: "password", Class: pressure.Credential}, Count: 5},
	}}
	rows := pressureRows(s)
	var capacity, credential bool
	for _, r := range rows {
		if r["value"] == "4  (capacity)" && r["state"] == "raised" {
			capacity = true
		}
		if r["value"] == "5  (credential)" && r["state"] == "normal" {
			credential = true
		}
	}
	if !capacity || !credential {
		t.Fatalf("refusals lost their class or state: %#v", rows)
	}
}

func TestPressureView_NoLimitIsNotShownAsZero(t *testing.T) {
	rows := pressureRows(pressure.Snapshot{Sessions: pressure.Row{Value: 3}})
	if got := rows[1]["value"]; got != "3  (no limit)" {
		t.Fatalf("unconfigured cap rendered as a zero ceiling: %v", got)
	}
}

func TestPressureDecode_AnUnknownClassIsNotReadAsCapacity(t *testing.T) {
	s := decodePressure(map[string]any{"denials": []any{map[string]any{
		"reason": "future-reason", "class": "new-class", "count": int64(2),
	}}})
	if len(s.Denials) != 1 || s.Denials[0].Class != pressure.Credential {
		t.Fatalf("unknown refusal class was guessed as capacity: %#v", s.Denials)
	}
}

func TestPressureDecode_TheViewSurvivesTheWire(t *testing.T) {
	s := decodePressure(map[string]any{
		"taken_at":         int64(1_780_000_000_000),
		"sessions":         map[string]any{"value": int64(9), "cap": int64(12)},
		"per_user":         []any{map[string]any{"subject": "42", "value": int64(7), "cap": int64(8)}},
		"per_user_omitted": int64(17), "leases_omitted": int64(3),
		"denials_omitted": int64(5), "throttled_omitted": int64(2),
	})
	if s.TakenAt.IsZero() || s.PerUserOmitted != 17 || s.LeasesOmitted != 3 ||
		s.DenialsOmitted != 5 || s.ThrottledOmitted != 2 {
		t.Fatalf("the wire dropped a timestamp or a remainder: %#v", s)
	}
	rows := pressureRows(s)
	for _, want := range []string{"and 17 more", "and 3 more", "and 5 more", "and 2 more"} {
		found := false
		for _, r := range rows {
			if r["value"] == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the view hid %q", want)
		}
	}
}

func TestPressureMalformedReplyIsUnavailableAndCannotEraseLastGoodReading(t *testing.T) {
	good := map[string]any{"sessions": map[string]any{"value": int64(3), "cap": int64(10)}}
	snap, err := pressureSnapshotFromWire(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []any{
		"not a snapshot", map[string]any{},
		map[string]any{"sessions": "not a row"},
		map[string]any{"sessions": map[string]any{"value": "3"}},
		map[string]any{"sessions": map[string]any{"value": float64(3.5)}},
		map[string]any{"sessions": map[string]any{"value": int64(3)}, "denials": "not a list"},
		map[string]any{"sessions": map[string]any{"value": int64(3)}, "denials": []any{"not a row"}},
	} {
		_, err := pressureSnapshotFromWire(malformed)
		if err == nil {
			t.Fatalf("unreadable pressure reply looked successful: %T", malformed)
		}
		p := newPressureView()
		p.apply(&Host{}, pressure.Snapshot{}, err, time.Now())
		if p.model.At(0)["measure"] != "unavailable" {
			t.Fatal("malformed first reply looked calm")
		}
		p.apply(&Host{}, snap, nil, time.Now().Add(-time.Minute))
		p.apply(&Host{}, pressure.Snapshot{}, err, time.Now())
		if !p.have || p.model.At(1)["value"] != "3 / 10" || p.failure == "" {
			t.Fatal("malformed refresh erased or passed off stale figures as live")
		}
	}
}
