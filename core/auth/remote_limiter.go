package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE REMOTE LIMITER — how many times an address may be refused before it is
// shut out, and the record of every refusal.
//
// A remote connection that ends in a denial (an unregistered key, a wrong
// device, a failed sign-in, a protocol violation) counts once against its
// source PREFIX: an IPv4 address, or the IPv6 /64 a host is usually given
// whole. After BlockAfter consecutive denials the prefix is blocked for
// BlockFor, measured from the denial that reached the threshold; a sign-in
// from the prefix resets its count. The block is in the store, so a restart
// keeps it, and an admin can lift it early (Unblock).
//
// EVERY DENIAL IS RECORDED, OR NOTHING IS ADMITTED. A denial is written as an
// audit row (remote_access_denied) and a counter step, in one transaction, and
// claimed by its event id (remote_denial_events) so a replay cannot count it
// twice. Before that transaction runs, the denial is queued in memory and
// appended to a spill file, fsynced. If the transaction fails, remote
// admission PAUSES: every new remote connection is refused, uncounted, until
// a probe has replayed the queue. So a store that cannot take a write cannot
// become a window for unlimited guesses, and a crash while paused loses no
// denial: the next start loads the spill file and stays paused until it has
// replayed it.

// DenialReason says why a remote connection was refused.
type DenialReason string

// The reasons a remote connection is refused and counted.
const (
	DenialKeyNotRegistered   DenialReason = "key_not_registered"
	DenialDeviceMismatch     DenialReason = "device_mismatch"
	DenialDeviceProofInvalid DenialReason = "device_proof_invalid"
	DenialDeviceRevoked      DenialReason = "device_revoked"
	DenialLoginFailed        DenialReason = "login_failed"
	DenialLoginUserMismatch  DenialReason = "login_user_mismatch"
	DenialProtocolViolation  DenialReason = "protocol_violation"
)

// Denial is one refused remote connection.
type Denial struct {
	EventID      string       `json:"event_id"`
	Prefix       string       `json:"prefix"`
	IP           string       `json:"ip"`
	At           int64        `json:"at"`
	Reason       DenialReason `json:"reason"`
	OfferedKeyFP string       `json:"offered_key_fp,omitempty"`
	UserID       int64        `json:"user_id,omitempty"`
}

// LimiterConfig is how the remote limiter counts and where it spills.
type LimiterConfig struct {
	// BlockAfter consecutive denials block a prefix; zero means 3.
	BlockAfter int
	// BlockFor is how long the block lasts; zero means 24h.
	BlockFor time.Duration
	// SpillPath is the file pending denials are appended to before they are
	// written to the store. Required.
	SpillPath string
	// RetryFirst and RetryMax bound the probe's backoff while paused; zero
	// means 1s and 30s.
	RetryFirst, RetryMax time.Duration
}

// RemoteLimiter counts remote denials per source prefix and decides admission.
// Safe for concurrent use.
type RemoteLimiter struct {
	svc *Service
	cfg LimiterConfig

	mu      sync.Mutex
	pending []Denial // not yet in the store, oldest first; mirrors the spill file
	paused  bool
	// pauseWhy is why admission is paused, for the admin's screen.
	pauseWhy string
	probing  bool
	// spillDirty is set when an append to the spill file failed, and may have
	// left a torn line: the next write replaces the whole file from pending
	// rather than appending behind it.
	spillDirty bool
	closed     chan struct{}
	wg         sync.WaitGroup

	// applyMu orders every store write that moves a prefix's count, denials
	// and resets alike, so a sign-in cannot reset a count before a denial
	// that came first has been counted.
	applyMu sync.Mutex

	// Test seams; nil in use.
	hookAdmitRead   func()                       // between Admit's pause check and its store read
	hookBeforeApply func(Denial)                 // in Deny, after queueing, before its store write
	hookSpillWrite  func([]byte) ([]byte, error) // what an append writes, and whether it fails
}

// PrefixOf is the prefix an address is counted and blocked under: the IPv4
// address itself (/32), or the IPv6 /64 around it. Anything that is not an IP
// address is its own prefix.
func PrefixOf(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return netip.PrefixFrom(a, 32).String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// NewRemoteLimiter builds the limiter over s, loading any denials a previous
// start left in the spill file. If there are some, it starts PAUSED and
// replays them before anything is admitted.
func (s *Service) NewRemoteLimiter(cfg LimiterConfig) (*RemoteLimiter, error) {
	if cfg.SpillPath == "" {
		return nil, errors.New("auth: remote limiter: no spill path")
	}
	if cfg.BlockAfter <= 0 {
		cfg.BlockAfter = 3
	}
	if cfg.BlockFor <= 0 {
		cfg.BlockFor = 24 * time.Hour
	}
	if cfg.RetryFirst <= 0 {
		cfg.RetryFirst = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 30 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SpillPath), 0o700); err != nil {
		return nil, fmt.Errorf("auth: remote limiter: spill directory: %w", err)
	}
	// Its own entry in ITS parent, when MkdirAll just made it.
	if err := syncDir(filepath.Dir(filepath.Dir(cfg.SpillPath))); err != nil {
		return nil, err
	}
	pending, err := readSpill(cfg.SpillPath)
	if err != nil {
		return nil, err
	}
	l := &RemoteLimiter{svc: s, cfg: cfg, pending: pending, closed: make(chan struct{})}
	if len(pending) > 0 {
		l.paused = true
		l.pauseWhy = "refusals from before the restart are still being recorded"
		l.startProbe()
	}
	return l, nil
}

// Close stops the probe. Pending denials stay in the spill file for the next
// start.
func (l *RemoteLimiter) Close() {
	l.mu.Lock()
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	l.mu.Unlock()
	l.wg.Wait()
}

// Admit says whether a new remote connection from ip may begin: false while
// admission is paused, or while ip's prefix is blocked. The reason names
// which; neither is counted against anyone.
func (l *RemoteLimiter) Admit(ctx context.Context, ip string) (bool, string) {
	l.mu.Lock()
	paused := l.paused
	l.mu.Unlock()
	if paused {
		return false, "remote admission is paused: denials cannot be recorded"
	}
	if l.hookAdmitRead != nil {
		l.hookAdmitRead()
	}
	block, err := l.svc.store.RemoteIPBlocks.OnCtx(ctx).With(meta.BlockPrefix, PrefixOf(ip)).Get()
	// The pause is checked again at the decision: a denial that failed to be
	// recorded while the read ran must not be followed by an admission.
	if l.Paused() {
		return false, "remote admission is paused: denials cannot be recorded"
	}
	if errors.Is(err, dao.ErrNoRows) {
		return true, ""
	}
	if err != nil {
		// Fail closed: a store that cannot answer cannot say the address is
		// not blocked.
		return false, "remote admission cannot read the blocks"
	}
	if block.BlockedUntil > l.svc.now().Unix() {
		return false, "blocked until " + time.Unix(block.BlockedUntil, 0).UTC().Format(time.RFC3339)
	}
	return true, ""
}

// Deny records a refused remote connection and counts it against its prefix.
// It returns once the denial is durable in the spill file; the store write
// follows at once, or, if it fails, remote admission pauses until a probe has
// written it. The returned error is only a spill failure.
func (l *RemoteLimiter) Deny(ip string, reason DenialReason, offeredKeyFP string, userID int64) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return fmt.Errorf("auth: remote limiter: event id: %w", err)
	}
	d := Denial{EventID: hex.EncodeToString(id), Prefix: PrefixOf(ip), IP: ip,
		At: l.svc.now().Unix(), Reason: reason, OfferedKeyFP: offeredKeyFP, UserID: userID}

	l.mu.Lock()
	l.pending = append(l.pending, d)
	spillErr := l.spillAppend(d)
	if spillErr != nil {
		// The memory queue still holds it and admission pauses below: no
		// further guess can arrive while it is in doubt.
		l.paused = true
		l.pauseWhy = "the refusal could not be written to the spill file: " + spillErr.Error()
	}
	paused := l.paused
	l.mu.Unlock()

	if paused {
		// Already paused: the probe replays in order, this one included.
		l.startProbe()
		return spillErr
	}
	if l.hookBeforeApply != nil {
		l.hookBeforeApply(d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.applyMu.Lock()
	err := l.apply(ctx, d)
	l.applyMu.Unlock()
	if err != nil {
		l.mu.Lock()
		l.paused = true
		l.pauseWhy = "the refusal could not be recorded: " + err.Error()
		l.mu.Unlock()
		l.startProbe()
		return spillErr
	}
	l.mu.Lock()
	l.drop(d.EventID)
	l.mu.Unlock()
	return spillErr
}

// Succeeded resets ip's prefix count: a sign-in from it ends the run of
// consecutive denials. A block in force is not lifted by it.
//
// In ORDER with the denials: every denial of the prefix queued before this
// call is counted first, so a sign-in cannot reset a count ahead of a denial
// that came before it. If one of them cannot be written, the count is left
// as it is and the error returned; the pause that follows replays it.
func (l *RemoteLimiter) Succeeded(ctx context.Context, ip string) error {
	prefix := PrefixOf(ip)
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.mu.Lock()
	var earlier []Denial
	for _, d := range l.pending {
		if d.Prefix == prefix {
			earlier = append(earlier, d)
		}
	}
	l.mu.Unlock()
	for _, d := range earlier {
		if err := l.apply(ctx, d); err != nil {
			l.mu.Lock()
			l.paused = true
			l.mu.Unlock()
			l.startProbe()
			return err
		}
		l.mu.Lock()
		l.drop(d.EventID)
		l.mu.Unlock()
	}
	return l.svc.store.RemoteIPBlocks.OnCtx(ctx).With(meta.BlockPrefix, prefix).
		Set(meta.BlockFailures, int64(0)).Update()
}

// Unblock lifts prefix's block and resets its count, recording who did it.
func (l *RemoteLimiter) Unblock(ctx context.Context, byUserID int64, prefix, ip string) error {
	return l.svc.inTx(ctx, func(tx *dao.Transaction) error {
		now := l.svc.now().Unix()
		if err := l.svc.store.RemoteIPBlocks.On(tx).With(meta.BlockPrefix, prefix).
			Set(meta.BlockFailures, int64(0)).Set(meta.BlockUntil, int64(0)).
			Set(meta.BlockUnblockedBy, byUserID).Set(meta.BlockUnblockedAt, now).Update(); err != nil {
			return err
		}
		return l.svc.AuditTx(tx, byUserID, ip, "remote_ip_unblocked", "prefix="+prefix)
	})
}

// Paused reports whether remote admission is paused.
func (l *RemoteLimiter) Paused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

// PauseReason is why remote admission is paused, "" while it is not.
func (l *RemoteLimiter) PauseReason() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.paused {
		return ""
	}
	return l.pauseWhy
}

// apply writes one denial durably: its claim, its audit row and its counter
// step, in one transaction, or nothing. A denial already claimed (a replay
// after a crash between the commit and the spill rewrite) changes nothing.
func (l *RemoteLimiter) apply(ctx context.Context, d Denial) error {
	return l.svc.inTx(ctx, func(tx *dao.Transaction) error {
		_, err := l.svc.store.RemoteDenials.On(tx).With(meta.DenialEventID, d.EventID).Get()
		if err == nil {
			return nil // already applied
		}
		if !errors.Is(err, dao.ErrNoRows) {
			return err
		}
		block, err := l.svc.store.RemoteIPBlocks.On(tx).With(meta.BlockPrefix, d.Prefix).Get()
		fresh := errors.Is(err, dao.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		failures := int64(1)
		blockedUntil := int64(0)
		if !fresh {
			failures = block.ConsecutiveFailures + 1
			blockedUntil = block.BlockedUntil
		}
		if failures >= int64(l.cfg.BlockAfter) {
			// From the denial's own time, not the replay's: an outage does
			// not stretch a block.
			if until := d.At + int64(l.cfg.BlockFor/time.Second); until > blockedUntil {
				blockedUntil = until
			}
		}
		detail := fmt.Sprintf("reason=%s prefix=%s failures=%d", d.Reason, d.Prefix, failures)
		if d.OfferedKeyFP != "" {
			detail += " key=" + d.OfferedKeyFP
		}
		if blockedUntil > 0 && failures >= int64(l.cfg.BlockAfter) {
			detail += " blocked_until=" + time.Unix(blockedUntil, 0).UTC().Format(time.RFC3339)
		}
		auditID, err := l.svc.auditTxRecordID(tx, AuditRecord{UserID: d.UserID, IP: d.IP,
			Action: "remote_access_denied", Detail: detail})
		if err != nil {
			return err
		}
		if _, err := l.svc.store.RemoteDenials.On(tx).
			Set(meta.DenialEventID, d.EventID).Set(meta.DenialPrefix, d.Prefix).
			Set(meta.DenialOccurredAt, d.At).Set(meta.DenialReason, string(d.Reason)).
			Set(meta.DenialOfferedKeyFP, d.OfferedKeyFP).Set(meta.DenialUserID, d.UserID).
			Set(meta.DenialAuditID, auditID).Insert(); err != nil {
			return err
		}
		if fresh {
			_, err = l.svc.store.RemoteIPBlocks.On(tx).
				Set(meta.BlockPrefix, d.Prefix).Set(meta.BlockFailures, failures).
				Set(meta.BlockLastFailureAt, d.At).Set(meta.BlockUntil, blockedUntil).Insert()
			return err
		}
		return l.svc.store.RemoteIPBlocks.On(tx).With(meta.BlockPrefix, d.Prefix).
			Set(meta.BlockFailures, failures).Set(meta.BlockLastFailureAt, d.At).
			Set(meta.BlockUntil, blockedUntil).Update()
	})
}

// startProbe replays the pending denials in order until the queue is empty,
// then resumes admission. One probe runs at a time.
func (l *RemoteLimiter) startProbe() {
	l.mu.Lock()
	if l.probing {
		l.mu.Unlock()
		return
	}
	select {
	case <-l.closed:
		l.mu.Unlock()
		return
	default:
	}
	l.probing = true
	l.wg.Add(1)
	l.mu.Unlock()
	go l.probe()
}

func (l *RemoteLimiter) probe() {
	defer l.wg.Done()
	wait := l.cfg.RetryFirst
	for {
		if l.replay() {
			return
		}
		select {
		case <-l.closed:
			l.mu.Lock()
			l.probing = false
			l.mu.Unlock()
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, l.cfg.RetryMax)
	}
}

// replay writes the pending denials oldest first and reports whether the
// queue is empty; then admission resumes. It stops at the first failure.
func (l *RemoteLimiter) replay() bool {
	for {
		l.mu.Lock()
		if len(l.pending) == 0 {
			// Resume only once the spill file agrees the queue is empty.
			if err := writeSpill(l.cfg.SpillPath, nil); err != nil {
				l.mu.Unlock()
				return false
			}
			l.spillDirty = false
			l.paused = false
			l.pauseWhy = ""
			l.probing = false
			l.mu.Unlock()
			return true
		}
		next := l.pending[0]
		l.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		l.applyMu.Lock()
		err := l.apply(ctx, next)
		l.applyMu.Unlock()
		cancel()
		if err != nil {
			l.mu.Lock()
			l.pauseWhy = "the refusal could not be recorded: " + err.Error()
			l.mu.Unlock()
			return false
		}
		l.mu.Lock()
		l.drop(next.EventID)
		l.mu.Unlock()
	}
}

// drop removes an applied denial from the queue and rewrites the spill file.
// Caller holds mu. A rewrite that fails leaves the applied denial in the file,
// which is harmless: its replay finds the claim and changes nothing.
func (l *RemoteLimiter) drop(eventID string) {
	for i, d := range l.pending {
		if d.EventID == eventID {
			l.pending = append(l.pending[:i:i], l.pending[i+1:]...)
			break
		}
	}
	if writeSpill(l.cfg.SpillPath, l.pending) == nil {
		l.spillDirty = false
	}
}

// spillAppend makes d durable in the spill file. Normally an append; after an
// append failed (which may have left a torn line) a full rewrite from pending,
// which d is already in, so a later record never sits behind a torn one. Caller
// holds mu.
func (l *RemoteLimiter) spillAppend(d Denial) error {
	if l.spillDirty {
		if err := writeSpill(l.cfg.SpillPath, l.pending); err != nil {
			return err
		}
		l.spillDirty = false
		return nil
	}
	if err := appendSpill(l.cfg.SpillPath, d, l.hookSpillWrite); err != nil {
		l.spillDirty = true
		return err
	}
	return nil
}

// --- the spill file: JSON lines, 0600 ----------------------------------------

func readSpill(path string) ([]Denial, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: remote limiter: spill: %w", err)
	}
	defer f.Close()
	var out []Denial
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var d Denial
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil || d.EventID == "" {
			// A torn line: a crash mid-append, whose denial was never
			// acknowledged. Skipped, not stopped at: the writer never appends
			// behind one, but a later complete record must survive regardless.
			continue
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("auth: remote limiter: spill: %w", err)
	}
	return out, nil
}

func appendSpill(path string, d Denial, hook func([]byte) ([]byte, error)) error {
	line, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("auth: remote limiter: spill: %w", err)
	}
	data := append(line, '\n')
	var hookErr error
	if hook != nil {
		data, hookErr = hook(data)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("auth: remote limiter: spill: %w", err)
	}
	if hookErr != nil {
		f.Close()
		return fmt.Errorf("auth: remote limiter: spill: %w", hookErr)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("auth: remote limiter: spill: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A new file's directory entry is durable only once its directory is.
	if created {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

// syncDir fsyncs a directory, so an entry created or renamed in it survives a
// power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("auth: remote limiter: spill directory: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("auth: remote limiter: spill directory: %w", err)
	}
	return nil
}

// writeSpill replaces the spill file with ds, atomically: temp file, fsync,
// rename.
func writeSpill(path string, ds []Denial) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, d := range ds {
		line, err := json.Marshal(d)
		if err != nil {
			f.Close()
			return err
		}
		_, _ = w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename is durable only once the directory is.
	return syncDir(filepath.Dir(path))
}
