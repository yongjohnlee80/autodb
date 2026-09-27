package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// Recovering the attempts a dead process left running.
//
// A process that dies between recording an attempt and recording its outcome
// leaves the history row `running` with nobody to finish it: the writer went
// with the process. Nothing else would ever settle it, so "every attempt
// reaches one disposition" would be false for that statement forever.
//
// THE OWNER IS KNOWN, AND SO IS ITS DEATH. Every attempt is stamped with the
// epoch of the instance lease its process held, and the lease is exclusive and
// held by the operating system for the process's lifetime. So while this
// daemon holds it, an attempt stamped with ANY other epoch belongs to a process
// that is gone — however long ago, however it died. That needs no clock and no
// timeout, and it is what makes a periodic pass safe, not only a startup one:
// the pass can never meet this process's own live attempts as foreign.
//
// Exactly these rows are settled, and no others:
//
//	disposition = '' AND status = 'running' AND attempt_owner <> <this epoch>
//
// status = 'running' is what keeps a row that FINISHED before schema script
// 000003 out: it has disposition '' for good and is not an attempt in this
// sense. An empty owner is a row still running from a release before 000003,
// whose process is gone by the same argument.
//
// What it writes is the one terminal the evidence supports: unknown. The
// statement may or may not have run before its process died. It goes through
// the same compare-and-set as every terminal, so two passes — or a pass racing
// a late writer — settle a row once.

// deadOwnerNote is the recorded error text of a recovered attempt.
const deadOwnerNote = "the process that ran this statement ended before recording its outcome"

// maxUnknownIDsPerLine caps how many attempt ids one warning line names.
const maxUnknownIDsPerLine = 20

// RecoverDeadAttempts settles every running attempt whose owner is not this
// process, and reports how many it settled: recent (made after 000003, which
// the unknown warning counts) and legacy (from before, which it does not).
//
// Safe at any time, for the reason above; it pages by id from the start each
// pass, since a running row is rare and a pass that finds none reads one page.
func (e *Engine) RecoverDeadAttempts(ctx context.Context) (recent, legacy int) {
	if !e.history {
		return 0, 0 // no durable per-attempt record to settle
	}
	var recentIDs []string
	for after := int64(0); ; {
		q := e.store.History.OnCtx(ctx).OrderBy(dao.Asc(meta.HistByID)).
			With(meta.HistDisposition, "").With(meta.HistStatus, string(StatusRunning))
		if after > 0 {
			q = q.WithPredicate(dao.Gt(string(meta.HistID), after))
		}
		rows, err := q.Limit(uint64(maxReconcileBatch)).Select()
		if err != nil {
			e.logf("attempt recovery: reading running attempts: %v", err)
			break
		}
		for _, r := range rows {
			after = max(after, r.ID)
			if r.AttemptOwner == e.ownerEpoch {
				continue // this process's own: a live statement
			}
			settled, err := e.settleDeadAttempt(ctx, r)
			if err != nil {
				e.logf("attempt recovery: history row %d: %v", r.ID, err)
				continue
			}
			if !settled {
				continue
			}
			if r.AttemptID == "" {
				legacy++
			} else {
				recent++
				recentIDs = append(recentIDs, r.AttemptID)
				e.unknowns.note(r.AttemptID)
			}
		}
		if len(rows) < maxReconcileBatch {
			break
		}
	}
	// LOUD for a recent one: an unknown is supposed to be rare enough that
	// one is an event, and the ids are what an operator looks up.
	for i := 0; i < len(recentIDs); i += maxUnknownIDsPerLine {
		j := min(i+maxUnknownIDsPerLine, len(recentIDs))
		e.logf("WARNING: %d statement attempt(s) settled as unknown — %s — attempts: %s",
			recent, deadOwnerNote, strings.Join(recentIDs[i:j], " "))
	}
	// Quiet for a legacy one: the first start after an upgrade settles what
	// the release before it left running, and that is not an alarm.
	if legacy > 0 {
		e.logf("settled %d history row(s) left running by a release before dispositions, as unknown", legacy)
	}
	return recent, legacy
}

// settleDeadAttempt writes r's unknown terminal, and its audit row when this
// call is the one that settled it.
func (e *Engine) settleDeadAttempt(ctx context.Context, r *meta.HistoryEntry) (bool, error) {
	att := Attempt{ID: r.AttemptID, HistID: r.ID}
	detail := fmt.Sprintf("conn %d (%s, 0 row(s), 0ms): %s", r.ConnectionID, StatusUnknown, deadOwnerNote)
	if r.AttemptID == "" {
		detail += " (a row from before dispositions)"
	}
	var settled bool
	var conflict error
	err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		var serr error
		settled, serr = e.settleTx(tx, r.UserID, r.IP, r.ConnectionID, att, meta.DispositionUnknown,
			map[meta.HistoryField]any{meta.HistStatus: StatusUnknown, meta.HistError: deadOwnerNote})
		if errors.Is(serr, ErrDispositionConflict) {
			conflict = serr
			return nil // commit: the conflict's own audit row is in tx
		}
		if serr != nil || !settled {
			return serr
		}
		return e.auth.AuditTxRecord(tx, auth.AuditRecord{UserID: r.UserID, IP: r.IP,
			Action: "exec_result", Detail: detail, TxID: r.TxID, AttemptID: r.AttemptID, ConnID: r.ConnectionID})
	})
	if err != nil {
		return false, err
	}
	if conflict != nil {
		// Its owner is dead, so no live writer can have answered differently:
		// a conflict here is a writer that outlived its lease, and loud.
		return false, fmt.Errorf("a dead owner's attempt already had another disposition: %w", conflict)
	}
	return settled, nil
}
