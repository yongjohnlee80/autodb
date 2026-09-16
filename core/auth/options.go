package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// User preferences: a small JSON object per account, read at sign-in and
// written a key at a time.
//
// WHY A DOCUMENT AND NOT A COLUMN EACH. The set is open — the editor keyset is
// the first preference and will not be the last — and a schema migration per
// checkbox is a cost with no matching benefit. The price of a document is that
// a writer must not clobber keys it has never heard of, which is what the merge
// below exists to prevent.

// OptionEditorKeyset selects the editing profile the TUI builds its editors
// with. The values are the two the product offers.
const OptionEditorKeyset = "editor.keyset"

const (
	// KeysetVim is modal editing.
	KeysetVim = "vim"
	// KeysetTextEdit is the modeless, standard-shortcut profile. It is named
	// for what the requirement calls it; golib spells the same profile
	// KeysetStandard.
	KeysetTextEdit = "textedit"
)

// knownOptions is the vocabulary this build understands, and the values each
// accepts. A key outside it is REFUSED on write — a typo would otherwise be
// stored forever, read by nothing, and never reported.
//
// It does not govern READS: a key written by a newer build is unknown here and
// must survive, which is exactly what the merge preserves.
var knownOptions = map[string][]string{
	OptionEditorKeyset: {KeysetVim, KeysetTextEdit},
}

// UserOptions reads an account's preferences.
//
// Unknown keys are returned as they were stored rather than dropped, so a
// downgrade reads a newer build's preferences without destroying them.
func (s *Service) UserOptions(ctx context.Context, token string) (map[string]string, error) {
	id, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return nil, err
	}
	row, err := s.store.Users.OnCtx(ctx).With(meta.UserID, id.UserID()).Get()
	if err != nil {
		return nil, err
	}
	return decodeOptions(row.Options)
}

// SetUserOption writes ONE preference for the caller's own account.
//
// THE MERGE IS INSIDE THE TRANSACTION, and that is the whole point. Two
// sessions changing two different preferences must both survive: reading the
// document outside a transaction, editing it, and writing the whole thing back
// is a lost update whenever the two overlap, and the loser has no way to know.
// Reading and writing under the guard row means the second writer sees the
// first one's committed document and merges into it.
//
// A per-key merge rather than a version check on the whole document, because a
// version check would REFUSE the second writer for touching an unrelated key —
// correct, but a conflict the operator cannot act on and did not cause.
func (s *Service) SetUserOption(ctx context.Context, token, key, value, ip string) error {
	id, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return err
	}
	allowed, known := knownOptions[key]
	if !known {
		return fmt.Errorf("auth: unknown preference %q", key)
	}
	if !contains(allowed, value) {
		return fmt.Errorf("auth: preference %q does not accept %q (accepted: %s)",
			key, value, strings.Join(allowed, ", "))
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.lockGuardRow(tx); err != nil {
			return err
		}
		row, err := s.store.Users.On(tx).With(meta.UserID, id.UserID()).Get()
		if err != nil {
			return err
		}
		opts, err := decodeOptions(row.Options)
		if err != nil {
			return err
		}
		opts[key] = value
		encoded, err := encodeOptions(opts)
		if err != nil {
			return err
		}
		if err := s.store.Users.On(tx).With(meta.UserID, id.UserID()).
			Set(meta.UserOptions, encoded).
			Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
			return err
		}
		return s.AuditTx(tx, id.UserID(), ip, "option_set", key+"="+value)
	})
}

// decodeOptions reads the stored document. An empty or absent column is an
// empty set rather than an error: v17 defaults the column to "{}", and a row
// written before that default existed should still read.
func decodeOptions(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("auth: preferences are not a JSON object: %w", err)
	}
	return out, nil
}

func encodeOptions(opts map[string]string) (string, error) {
	b, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("auth: encoding preferences: %w", err)
	}
	return string(b), nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
