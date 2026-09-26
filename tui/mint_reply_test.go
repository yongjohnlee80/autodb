package tui

import (
	"errors"
	"testing"
)

func TestMintReplyRejectsAnUnshowableSuccessAsUncertain(t *testing.T) {
	for _, reply := range []any{nil, "wrong shape", map[string]any{}, map[string]any{"name": "example"}, map[string]any{"secret": "fake-only"}} {
		out, stale, err := decodeMintReply("example", reply)
		if !errors.Is(err, errMintReplyUncertain) || out.Name != "example" || len(stale) != 0 {
			t.Fatalf("malformed successful mint lost its reconciliation name: type %T", reply)
		}
	}
	out, stale, err := decodeMintReply("example", map[string]any{"name": "example", "secret": "fake-only"})
	if err != nil || len(stale) != 0 || out.Name != "example" || out.Secret == "" {
		t.Fatal("valid show-once answer was rejected")
	}
	if _, missing, err := decodeMintReply("example", map[string]any{"stale_approval": true}); err == nil || len(missing) != 0 {
		t.Fatal("empty stale-approval reply was treated as a successful mint")
	}
}
