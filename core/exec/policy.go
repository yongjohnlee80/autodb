package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// The reloadable policy: the bounds an operator can change on a running
// daemon, and the one path that changes them.
//
// WHAT THIS REPLACES was a setter reachable only from a program embedding the
// engine. It changed the connection budget alone, left the timeouts behind,
// and forgot everything on restart -- so the accepted policy's requirement
// that the bounds publish TOGETHER, and survive, was satisfied by nothing. An
// operator who raised the budget after an incident would find it back at the
// old number the next morning, with no record that it had ever been changed.

// PolicySpec is a complete replacement for the reloadable bounds.
//
// COMPLETE, not a patch. A partial update would need a way to say "leave this
// one alone", and the difference between "unset" and "zero" is exactly where a
// production-safety bound goes missing. Every field is required and validated
// together, so what an operator submits is what the engine will be running.
type PolicySpec struct {
	SessionIdleTimeout   time.Duration
	IdleInTxTimeout      time.Duration
	MaxTxDuration        time.Duration
	MaxTxDurationCeiling time.Duration
	// MaxTargetConns is the aggregate production-connection budget. Ignored
	// when this engine has no ledger (an embedded engine with no front door);
	// required otherwise.
	MaxTargetConns int
}

// enginePolicy is what the running engine reads. Published as ONE immutable
// value behind an atomic pointer, so a reader takes a coherent set rather
// than assembling one out of four fields being written underneath it.
type enginePolicy struct {
	sessionIdle  time.Duration
	tx           txLimits
	debugIdle    time.Duration
	maxTxCeiling time.Duration
	// generation increments once per successful reload. It is what makes a
	// readout answerable: two observers comparing settings can tell whether
	// they saw the same publication or two different ones.
	generation uint64
}

// policyStoreKey is where the durable override lives. One row, one JSON
// document, replaced whole -- so a reload cannot half-apply, and a restart
// reads either the previous policy or the new one and never a mixture.
const policyStoreKey = "exec.policy"

// storedPolicy is the durable form. Durations are milliseconds because a
// stored Go duration string is a parsing decision nobody should have to make
// again in five years.
//
// IT CARRIES NO SECRET. The provenance fields are a user ROW ID and a
// timestamp; the operator's token is not part of the record and neither is
// their address, which belongs in the audit row where access to it is
// governed.
type storedPolicy struct {
	SessionIdleMS  int64  `json:"session_idle_ms"`
	IdleInTxMS     int64  `json:"idle_in_tx_ms"`
	MaxTxMS        int64  `json:"max_tx_ms"`
	MaxTxCeilingMS int64  `json:"max_tx_ceiling_ms"`
	MaxTargetConns int    `json:"max_target_conns"`
	Generation     uint64 `json:"generation"`
	UpdatedAt      int64  `json:"updated_at"`
	UpdatedBy      int64  `json:"updated_by"`
}

func (s PolicySpec) stored(generation uint64, at, by int64) storedPolicy {
	return storedPolicy{
		SessionIdleMS:  s.SessionIdleTimeout.Milliseconds(),
		IdleInTxMS:     s.IdleInTxTimeout.Milliseconds(),
		MaxTxMS:        s.MaxTxDuration.Milliseconds(),
		MaxTxCeilingMS: s.MaxTxDurationCeiling.Milliseconds(),
		MaxTargetConns: s.MaxTargetConns,
		Generation:     generation,
		UpdatedAt:      at,
		UpdatedBy:      by,
	}
}

func (p storedPolicy) spec() PolicySpec {
	return PolicySpec{
		SessionIdleTimeout:   time.Duration(p.SessionIdleMS) * time.Millisecond,
		IdleInTxTimeout:      time.Duration(p.IdleInTxMS) * time.Millisecond,
		MaxTxDuration:        time.Duration(p.MaxTxMS) * time.Millisecond,
		MaxTxDurationCeiling: time.Duration(p.MaxTxCeilingMS) * time.Millisecond,
		MaxTargetConns:       p.MaxTargetConns,
	}
}

// ErrPolicyInvalid is returned for a spec that would describe a configuration
// the daemon refuses at startup.
var ErrPolicyInvalid = fmt.Errorf("exec: invalid policy")

// ValidatePolicy reports whether a spec would be accepted, without changing
// anything. An operator surface can offer a dry run, and it is what lets the
// cross-package coherence cell compare these rules against core/config's.
func (e *Engine) ValidatePolicy(s PolicySpec) error { return e.validatePolicy(s) }

// validate checks the WHOLE spec before anything is written.
//
// The rules are the ones core/config enforces on the same values at load, and
// they are restated here rather than imported because core/config must not
// depend on this package and this package must not depend on it. That makes
// them two copies of one rule set, which is a thing that drifts -- so a
// cross-package cell in cmd/autodb asserts the two agree, and it is the reason
// the failure messages below are worth keeping recognisably similar.
//
// NOTHING IS MUTATED UNTIL THIS PASSES. A reload that validated field by field
// as it applied them would leave the engine running a configuration that is
// half the operator's and half the old one's, with no record of which.
func (e *Engine) validatePolicy(s PolicySpec) error {
	for _, b := range []struct {
		name string
		val  time.Duration
	}{
		{"session_idle_timeout", s.SessionIdleTimeout},
		{"idle_in_tx_timeout", s.IdleInTxTimeout},
		{"max_tx_duration", s.MaxTxDuration},
		{"max_tx_duration_ceiling", s.MaxTxDurationCeiling},
	} {
		if b.val <= 0 {
			return fmt.Errorf("%w: %s %s must be positive — an unbounded transaction on a live "+
				"database holds its locks until something else ends it", ErrPolicyInvalid, b.name, b.val)
		}
		// MILLISECOND-ALIGNED, because that is the resolution the durable form
		// keeps. A bound expressed more finely would RUN at the value given
		// and PERSIST as something else: a 1ns bound stores as zero, and the
		// next start refuses to boot on a policy the operator never wrote.
		//
		// Refused rather than rounded. Rounding would mean the daemon ran a
		// bound nobody chose while the audit row recorded the one they asked
		// for, and the difference would surface as a transaction ending early
		// for no reason anyone could find.
		if b.val%time.Millisecond != 0 {
			return fmt.Errorf("%w: %s (%v) is not a whole number of milliseconds, which is the "+
				"resolution the stored policy keeps; it would run at one value and persist as "+
				"another", ErrPolicyInvalid, b.name, b.val)
		}
	}
	if s.MaxTxDuration > s.MaxTxDurationCeiling {
		return fmt.Errorf("%w: max_tx_duration %s exceeds max_tx_duration_ceiling %s",
			ErrPolicyInvalid, s.MaxTxDuration, s.MaxTxDurationCeiling)
	}
	if e.poolMaxConnIdleTime > 0 && s.SessionIdleTimeout > e.poolMaxConnIdleTime {
		return fmt.Errorf("%w: session_idle_timeout (%v) exceeds pool_max_conn_idle_time (%v) — "+
			"an idle session would hold a backend checked out past the point the pool would have "+
			"closed it, so unused pools could never shrink to zero against the target",
			ErrPolicyInvalid, s.SessionIdleTimeout, e.poolMaxConnIdleTime)
	}
	// The sweep is what enforces every bound above. A deadline shorter than
	// the interval between sweeps is a deadline nothing looks at in time, and
	// the janitor's interval is fixed at startup -- so it is the reload that
	// has to respect it, not the other way round.
	if every := e.janitorEvery(); every > 0 && every >= s.IdleInTxTimeout {
		return fmt.Errorf("%w: idle_in_tx_timeout (%v) is not longer than the janitor interval (%v), "+
			"so a transaction could sit well past its deadline before anything looked",
			ErrPolicyInvalid, s.IdleInTxTimeout, every)
	}
	if e.targetPermits != nil && s.MaxTargetConns < 2 {
		return fmt.Errorf("%w: max_target_conns is %d, which leaves nothing for ordinary work — "+
			"one permit is reserved so a cancellation can still be delivered when every other "+
			"slot is spent. Use at least 2", ErrPolicyInvalid, s.MaxTargetConns)
	}
	return nil
}

// ReloadPolicy replaces the reloadable bounds on a running daemon, durably.
//
// Admin only, audited, validated before anything moves, written to the meta
// store in the same transaction as its audit row, and published to the running
// engine as one generation. What it does NOT do is close anything: lowering the
// budget below what is outstanding leaves every live session alone and stops
// granting new permits, so the number becomes true by attrition rather than by
// an outage in the middle of the working day.
func (e *Engine) ReloadPolicy(ctx context.Context, token string, spec PolicySpec, ip string) (Settings, error) {
	ident, err := e.auth.ValidateToken(ctx, token)
	if err != nil {
		return Settings{}, err
	}
	// The bounds decide how long anyone may hold a production connection, so
	// changing them is an administrative act and not a user preference.
	if ident.Role() != meta.RoleAdmin {
		return Settings{}, auth.ErrDenied
	}
	if err := e.validatePolicy(spec); err != nil {
		return Settings{}, err
	}

	// Serialised PER ENGINE. Two operators submitting at once would otherwise
	// each read the current generation, each write generation+1, and one
	// change would vanish while its audit row claimed it had landed.
	e.policyGuard.Lock()
	defer e.policyGuard.Unlock()

	cur := e.currentPolicy()
	next := &enginePolicy{
		sessionIdle: spec.SessionIdleTimeout,
		tx: txLimits{
			idleInTx:         spec.IdleInTxTimeout,
			maxTx:            spec.MaxTxDuration,
			serverBeltMargin: cur.tx.serverBeltMargin,
		},
		// DERIVED, never supplied. The debug bound is deprecated and no longer
		// selects anything; letting a reload set it separately would let an
		// operator store a number describing behaviour the runtime does not
		// have. Deriving it makes the two incapable of disagreeing.
		debugIdle:    spec.IdleInTxTimeout,
		maxTxCeiling: spec.MaxTxDurationCeiling,
		generation:   cur.generation + 1,
	}

	blob, err := json.Marshal(spec.stored(next.generation, e.now().Unix(), ident.UserID()))
	if err != nil {
		return Settings{}, err
	}

	// DURABLE FIRST. If the process dies between the store and the publish,
	// the next start reads the operator's policy and runs it; the reverse
	// order would run a change that silently reverted at the next restart,
	// which is the failure the old setter had.
	if err := dao.RunTx(ctx, func(tx *dao.Transaction) error {
		if uerr := e.store.KV.On(tx).
			Set(meta.KVKey, policyStoreKey).Set(meta.KVValue, string(blob)).Upsert(); uerr != nil {
			return uerr
		}
		// In the SAME transaction as the change it describes, so the trail
		// cannot record a reload that did not happen or miss one that did.
		return e.auth.AuditTx(tx, ident.UserID(), ip, "policy_reloaded", policyAuditDetail(cur, next, spec))
	}); err != nil {
		return Settings{}, err
	}

	e.publishPolicy(next, spec.MaxTargetConns)
	return e.Settings(), nil
}

// publishPolicy makes the new bounds live.
//
// When there is a ledger the pointer swap happens INSIDE its lock, so the
// budget and the timeouts become visible in the same instant to anyone taking
// a snapshot. Without a ledger there is only the pointer, and a single atomic
// store is already indivisible.
func (e *Engine) publishPolicy(next *enginePolicy, budget int) {
	if e.targetPermits == nil {
		e.policy.Store(next)
		return
	}
	// ONE GENERATION, CHOSEN ONCE AND GIVEN TO BOTH SIDES. The policy owns the
	// number; the ledger is told it rather than keeping a count of its own.
	// The only way this errors is a budget below two, and validatePolicy has
	// already refused that for every engine that HAS a ledger -- which is the
	// only way to reach this line. Returning an error here would be a second
	// path for a refusal that must happen before anything is written, and a
	// caller reaching it would already have a durable record of the change.
	_ = e.targetPermits.setBudgetWith(budget, next.generation, func() { e.policy.Store(next) })
}

// policyAuditDetail renders the change. Numbers only: no token, no spec
// struct dumped wholesale, and the before as well as the after, because "set
// to 8h" does not tell a reader whether anything changed.
func policyAuditDetail(from, to *enginePolicy, spec PolicySpec) string {
	return fmt.Sprintf(
		"generation %d -> %d: session_idle %v -> %v, idle_in_tx %v -> %v, max_tx %v -> %v, "+
			"max_tx_ceiling %v -> %v, max_target_conns -> %d",
		from.generation, to.generation,
		from.sessionIdle, to.sessionIdle,
		from.tx.idleInTx, to.tx.idleInTx,
		from.tx.maxTx, to.tx.maxTx,
		from.maxTxCeiling, to.maxTxCeiling,
		spec.MaxTargetConns)
}

// LoadDurablePolicy applies a previously reloaded policy over the
// configuration this engine was built with. The daemon calls it once at
// startup, after New and before serving.
//
// THIS IS WHAT MAKES A RELOAD SURVIVE A RESTART. Without it the stored row is
// a record of an intention nothing acts on, and an operator's 3am change is
// undone by the next deploy.
//
// A spec that no longer validates is REFUSED rather than partially applied:
// the stored policy was valid when it was written, so if it is not valid now
// something else changed underneath it -- the pool's idle time, the janitor's
// interval -- and running half of it would be a configuration nobody chose.
func (e *Engine) LoadDurablePolicy(ctx context.Context) error {
	raw, ok, err := e.store.GetMeta(ctx, policyStoreKey)
	if err != nil {
		return err
	}
	if !ok {
		// No override. The configured policy stands, which is the ordinary
		// case and not a problem.
		return nil
	}
	var stored storedPolicy
	if uerr := json.Unmarshal([]byte(raw), &stored); uerr != nil {
		return fmt.Errorf("exec: the stored policy could not be read (%w); "+
			"refusing to start on a policy nobody can see", uerr)
	}
	spec := stored.spec()
	if verr := e.validatePolicy(spec); verr != nil {
		return fmt.Errorf("exec: the stored policy is no longer valid: %w", verr)
	}
	cur := e.currentPolicy()
	e.publishPolicy(&enginePolicy{
		sessionIdle: spec.SessionIdleTimeout,
		tx: txLimits{
			idleInTx:         spec.IdleInTxTimeout,
			maxTx:            spec.MaxTxDuration,
			serverBeltMargin: cur.tx.serverBeltMargin,
		},
		debugIdle:    spec.IdleInTxTimeout,
		maxTxCeiling: spec.MaxTxDurationCeiling,
		generation:   stored.Generation,
	}, spec.MaxTargetConns)
	return nil
}

// ShowPolicy is the read half of the operator surface: what is in force right
// now, including where it came from.
//
// Authenticated but NOT admin-only. An operator diagnosing "why did my
// transaction end" needs to see the bounds that ended it, and the bounds are
// not a secret -- every one of them is already observable by holding a
// transaction open and timing the refusal. Changing them is the privileged
// act; reading them is what makes the privileged act reviewable.
func (e *Engine) ShowPolicy(ctx context.Context, token string) (map[string]any, error) {
	if _, err := e.auth.ValidateToken(ctx, token); err != nil {
		return nil, err
	}
	return PolicyView(e.Settings()), nil
}

// PolicyView renders settings for the wire.
//
// Milliseconds and plain numbers: a readout an operator can compare against
// what they submitted, in the same units they submitted it in. A view that
// answered in Go duration strings would make "did my change land" a parsing
// question.
func PolicyView(s Settings) map[string]any {
	return map[string]any{
		"generation":        s.PolicyGeneration,
		"session_idle_ms":   s.SessionIdleTimeout.Milliseconds(),
		"idle_in_tx_ms":     s.IdleInTxTimeout.Milliseconds(),
		"max_tx_ms":         s.MaxTxDuration.Milliseconds(),
		"max_tx_ceiling_ms": s.MaxTxDurationCeiling.Milliseconds(),
		// The ledger's own reading, so a caller sees what the budget IS as
		// well as what it was set to -- during a drain those differ, and the
		// difference is the whole reason an operator is looking.
		"max_target_conns":         s.TargetConns.Configured,
		"target_conns_effective":   s.TargetConns.Effective,
		"target_conns_outstanding": s.TargetConns.Outstanding,
		"ledger_generation":        s.TargetConns.Generation,
	}
}
