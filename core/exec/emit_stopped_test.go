package exec

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Arm encodes the six arms and their order; this table is the contract the
// loop consumes. The first row is the one lector caught: an empty query cut
// inside BEGIN has TxStatus T and NO statement — it must not read as pending.
func TestEmitStopped_ArmOrder(t *testing.T) {
	cause := errors.New("client gone")
	pgErr := &pgconn.PgError{Code: "22012"}
	cases := []struct {
		name string
		st   EmitStopped
		want EmitArm
	}{
		{"empty query inside BEGIN: no statement outranks T", EmitStopped{Cause: cause, TxStatus: TxStatusInTx}, ArmNoStatement},
		{"empty query in autocommit", EmitStopped{Cause: cause, TxStatus: TxStatusIdle}, ArmNoStatement},
		{"target error outranks T", EmitStopped{Cause: cause, Executed: true, TxStatus: TxStatusInTx, TargetErr: pgErr, Outcome: StatusError}, ArmFailed},
		{"T: pending", EmitStopped{Cause: cause, Executed: true, TxStatus: TxStatusInTx, Outcome: StatusPendingCommit}, ArmPending},
		{"E: aborted", EmitStopped{Cause: cause, Executed: true, TxStatus: TxStatusAborted, Outcome: StatusUnresolvable}, ArmAborted},
		{"observed complete", EmitStopped{Cause: cause, Executed: true, TxStatus: TxStatusIdle, Outcome: StatusOK}, ArmCompleted},
		{"unobserved tail", EmitStopped{Cause: cause, Executed: true, TxStatus: TxStatusIdle, Outcome: StatusUnresolvable}, ArmUnresolved},
	}
	for _, c := range cases {
		if got := c.st.Arm(); got != c.want {
			t.Errorf("%s: Arm() = %s, want %s", c.name, got, c.want)
		}
	}
	if !cases[6].st.Unresolved() || cases[0].st.Unresolved() {
		t.Fatal("Unresolved() must follow Arm(): true only for the sixth arm")
	}
	// The message is truthful about dispatch.
	if msg := cases[0].st.Error(); strings.Contains(msg, "after dispatch") || !strings.Contains(msg, "no statement ran") {
		t.Fatalf("empty-query Error() claims a dispatch: %q", msg)
	}
	if msg := cases[3].st.Error(); !strings.Contains(msg, "after dispatch") {
		t.Fatalf("executed Error() should say so: %q", msg)
	}
}
