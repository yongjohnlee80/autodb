package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

func newLimiter(t *testing.T, s *Service, spill string) *RemoteLimiter {
	t.Helper()
	l, err := s.NewRemoteLimiter(LimiterConfig{SpillPath: spill,
		RetryFirst: 10 * time.Millisecond, RetryMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewRemoteLimiter: %v", err)
	}
	t.Cleanup(l.Close)
	return l
}

func admitted(t *testing.T, l *RemoteLimiter, ip string) bool {
	t.Helper()
	ok, _ := l.Admit(context.Background(), ip)
	return ok
}

func blockOf(t *testing.T, store *meta.Store, prefix string) *meta.RemoteIPBlock {
	t.Helper()
	b, err := store.RemoteIPBlocks.OnCtx(context.Background()).With(meta.BlockPrefix, prefix).Get()
	if err != nil {
		t.Fatalf("block %s: %v", prefix, err)
	}
	return b
}

// failDenials installs, once and before any probe runs, an audit hook that
// refuses remote_access_denied rows while the returned flag is set. Toggling
// the flag, not the hook, keeps the probe goroutine free of a data race.
func failDenials(s *Service) *atomic.Bool {
	var fail atomic.Bool
	s.hookAuditWrite = func(action string) error {
		if action == "remote_access_denied" && fail.Load() {
			return errors.New("injected: the store refuses writes")
		}
		return nil
	}
	return &fail
}

func deny(t *testing.T, l *RemoteLimiter, ip string) {
	t.Helper()
	if err := l.Deny(ip, DenialKeyNotRegistered, "SHA256:x", 0); err != nil {
		t.Fatalf("Deny: %v", err)
	}
}

// An IPv4 address is its own prefix; IPv6 addresses share their /64.
func TestPrefixOfCountsIPv6ByItsSlash64(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.7":        "203.0.113.7/32",
		"::ffff:203.0.113.7": "203.0.113.7/32",
		"2001:db8:1:2::1":    "2001:db8:1:2::/64",
		"2001:db8:1:2:ff::9": "2001:db8:1:2::/64",
		"local":              "local",
	} {
		if got := PrefixOf(ip); got != want {
			t.Errorf("PrefixOf(%q) = %q, want %q", ip, got, want)
		}
	}
}

// Three consecutive denials block the prefix for 24 hours from the third; the
// block is recorded, other addresses are untouched, and it ends on time.
func TestThreeDenialsBlockAPrefixForADay(t *testing.T) {
	s, store, ck := newSvc(t)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	for i := 0; i < 2; i++ {
		deny(t, l, "203.0.113.7")
	}
	if !admitted(t, l, "203.0.113.7") {
		t.Fatal("blocked after two denials")
	}
	deny(t, l, "203.0.113.7")
	if admitted(t, l, "203.0.113.7") {
		t.Fatal("admitted after three denials")
	}
	if !admitted(t, l, "198.51.100.9") {
		t.Fatal("another address was blocked")
	}
	if b := blockOf(t, store, "203.0.113.7/32"); b.ConsecutiveFailures != 3 || b.BlockedUntil != ck.t.Add(24*time.Hour).Unix() {
		t.Fatalf("block %+v; want 3 failures until +24h", b)
	}
	if n := auditCount(t, store, "remote_access_denied"); n != 3 {
		t.Errorf("remote_access_denied rows: %d, want 3", n)
	}
	ck.t = ck.t.Add(24*time.Hour + time.Second)
	if !admitted(t, l, "203.0.113.7") {
		t.Fatal("still blocked after 24 hours")
	}
}

// IPv6 hosts in one /64 are counted together.
func TestDenialsFromOneSlash64AreCountedTogether(t *testing.T) {
	s, _, _ := newSvc(t)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	for _, ip := range []string{"2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:1:2:ff::3"} {
		deny(t, l, ip)
	}
	if admitted(t, l, "2001:db8:1:2::99") {
		t.Fatal("a fourth host in the /64 was admitted")
	}
	if !admitted(t, l, "2001:db8:1:3::1") {
		t.Fatal("the next /64 was blocked")
	}
}

// A sign-in from the prefix resets its count: denials must be consecutive.
func TestASuccessResetsTheCount(t *testing.T) {
	s, _, _ := newSvc(t)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	deny(t, l, "203.0.113.7")
	deny(t, l, "203.0.113.7")
	if err := l.Succeeded(context.Background(), "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	deny(t, l, "203.0.113.7")
	deny(t, l, "203.0.113.7")
	if !admitted(t, l, "203.0.113.7") {
		t.Fatal("blocked although a success came between the denials")
	}
}

// A block survives a restart (a new limiter over the same store), and an admin
// can lift it early; the unblock is audited and resets the count.
func TestABlockSurvivesARestartAndAnAdminCanLiftIt(t *testing.T) {
	s, store, _ := newSvc(t)
	spill := filepath.Join(t.TempDir(), "spill")
	l := newLimiter(t, s, spill)
	for i := 0; i < 3; i++ {
		deny(t, l, "203.0.113.7")
	}
	l.Close()
	again := newLimiter(t, s, spill)
	if admitted(t, again, "203.0.113.7") {
		t.Fatal("the block did not survive a restart")
	}
	if err := again.Unblock(context.Background(), 1, "203.0.113.7/32", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if !admitted(t, again, "203.0.113.7") {
		t.Fatal("still blocked after an admin unblock")
	}
	if b := blockOf(t, store, "203.0.113.7/32"); b.ConsecutiveFailures != 0 || b.UnblockedBy != 1 {
		t.Fatalf("after unblock: %+v", b)
	}
	if auditCount(t, store, "remote_ip_unblocked") != 1 {
		t.Error("the unblock was not audited")
	}
}

// A denial applied twice (a replay after a crash between the commit and the
// spill rewrite) is one claim, one audit row and one counter step.
func TestReplayingADenialCountsItOnce(t *testing.T) {
	s, store, _ := newSvc(t)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	d := Denial{EventID: "e1", Prefix: "203.0.113.7/32", IP: "203.0.113.7", At: s.now().Unix(), Reason: DenialLoginFailed}
	for i := 0; i < 2; i++ {
		if err := l.apply(context.Background(), d); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if b := blockOf(t, store, d.Prefix); b.ConsecutiveFailures != 1 {
		t.Fatalf("counted %d times, want 1", b.ConsecutiveFailures)
	}
	if n := auditCount(t, store, "remote_access_denied"); n != 1 {
		t.Fatalf("audit rows %d, want 1", n)
	}
}

// FAIL CLOSED: when a denial cannot be recorded, remote admission pauses for
// everyone, uncounted, until the pending denial has been written; then the
// count is right and admission resumes.
func TestADenialThatCannotBeRecordedPausesAdmission(t *testing.T) {
	s, store, _ := newSvc(t)
	fail := failDenials(s)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	fail.Store(true)
	deny(t, l, "203.0.113.7")
	if !l.Paused() || admitted(t, l, "198.51.100.9") {
		t.Fatal("a denial that could not be recorded left admission open")
	}
	if why := l.PauseReason(); !strings.Contains(why, "could not be recorded") {
		t.Fatalf("the pause's reason %q; want why refusals are not recorded", why)
	}
	fail.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for l.Paused() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if l.Paused() {
		t.Fatal("admission never resumed after the store recovered")
	}
	if why := l.PauseReason(); why != "" {
		t.Fatalf("resumed, the pause still has a reason: %q", why)
	}
	if b := blockOf(t, store, "203.0.113.7/32"); b.ConsecutiveFailures != 1 {
		t.Fatalf("after recovery the prefix counts %d, want 1", b.ConsecutiveFailures)
	}
}

// A crash while paused loses nothing: the spill file holds the denial, and
// the next start loads it, stays paused and replays it. Starting from two
// recorded failures, the third (unrecorded at the crash) blocks the prefix,
// from the third denial's own time.
func TestAPendingDenialSurvivesACrash(t *testing.T) {
	s, store, ck := newSvc(t)
	fail := failDenials(s)
	spill := filepath.Join(t.TempDir(), "spill")
	l := newLimiter(t, s, spill)
	deny(t, l, "203.0.113.7")
	deny(t, l, "203.0.113.7")
	third := ck.t
	fail.Store(true)
	deny(t, l, "203.0.113.7")
	l.Close() // the crash: the probe never wrote it
	if data, err := os.ReadFile(spill); err != nil || !strings.Contains(string(data), "203.0.113.7") {
		t.Fatalf("the spill file does not hold the pending denial: %q, %v", data, err)
	}
	fail.Store(false)
	ck.t = ck.t.Add(3 * time.Hour) // the outage lasted three hours

	again := newLimiter(t, s, spill)
	deadline := time.Now().Add(3 * time.Second)
	for again.Paused() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if again.Paused() {
		t.Fatal("the restart never replayed the spill file")
	}
	b := blockOf(t, store, "203.0.113.7/32")
	if b.ConsecutiveFailures != 3 || b.BlockedUntil != third.Add(24*time.Hour).Unix() {
		t.Fatalf("after the replay %+v; want 3 failures, blocked 24h from the third denial", b)
	}
	if admitted(t, again, "203.0.113.7") {
		t.Fatal("the replayed third denial did not block")
	}
	if data, _ := os.ReadFile(spill); len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("the spill file still holds %q after the replay", data)
	}
}

// An append that fails after writing half a line leaves a torn
// line; the next denial must not be appended behind it, or a restart that
// cannot read past the tear loses it. After the failed append the spill file
// is rewritten whole, so a restart finds BOTH pending denials.
func TestATornSpillAppendDoesNotHideTheNextDenial(t *testing.T) {
	s, store, _ := newSvc(t)
	fail := failDenials(s)
	spill := filepath.Join(t.TempDir(), "spill")
	l := newLimiter(t, s, spill)
	fail.Store(true) // both stay pending
	var tornOnce atomic.Bool
	l.hookSpillWrite = func(b []byte) ([]byte, error) {
		if tornOnce.CompareAndSwap(false, true) {
			return b[:len(b)/2], errors.New("injected: the disk filled mid-line")
		}
		return b, nil
	}
	_ = l.Deny("203.0.113.7", DenialKeyNotRegistered, "", 0) // torn
	deny(t, l, "203.0.113.7")                                // must survive
	l.Close()

	fail.Store(false)
	again := newLimiter(t, s, spill)
	deadline := time.Now().Add(3 * time.Second)
	for again.Paused() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b := blockOf(t, store, "203.0.113.7/32"); b.ConsecutiveFailures != 2 {
		t.Fatalf("after the restart the prefix counts %d, want both pending denials (2)", b.ConsecutiveFailures)
	}
}

// A spill file with a torn line in the middle still yields the complete
// records after it.
func TestTheSpillReaderSkipsATornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spill")
	body := `{"event_id":"a","prefix":"p","at":1}` + "\n" + `{"event_id":"b","pre` + "\n" + `{"event_id":"c","prefix":"p","at":3}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSpill(path)
	if err != nil || len(got) != 2 || got[0].EventID != "a" || got[1].EventID != "c" {
		t.Fatalf("readSpill: %+v, %v; want a and c", got, err)
	}
}

// A denial that fails to be recorded WHILE Admit reads the
// blocks pauses admission, and that Admit must not answer yes.
func TestAdmitRechecksThePauseAfterItsRead(t *testing.T) {
	s, _, _ := newSvc(t)
	fail := failDenials(s)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	fail.Store(true)
	l.hookAdmitRead = func() {
		l.hookAdmitRead = nil
		_ = l.Deny("198.51.100.9", DenialLoginFailed, "", 0) // pauses
	}
	if ok, why := l.Admit(context.Background(), "203.0.113.7"); ok {
		t.Fatalf("Admit answered yes although admission paused during its read (%q)", why)
	}
}

// A sign-in must not reset the count ahead of a denial that came
// before it. Two denials are counted; the third is queued but its own write is
// held back; the sign-in then arrives. The third is counted first (and blocks,
// three in a row), and only then is the count reset; the block stands.
func TestASuccessCannotResetAheadOfAnEarlierDenial(t *testing.T) {
	s, store, _ := newSvc(t)
	l := newLimiter(t, s, filepath.Join(t.TempDir(), "spill"))
	deny(t, l, "203.0.113.7")
	deny(t, l, "203.0.113.7")
	queued, release := make(chan struct{}), make(chan struct{})
	l.hookBeforeApply = func(Denial) {
		close(queued)
		<-release
	}
	done := make(chan struct{})
	go func() { deny(t, l, "203.0.113.7"); close(done) }()
	<-queued
	if err := l.Succeeded(context.Background(), "203.0.113.7"); err != nil {
		t.Fatalf("Succeeded: %v", err)
	}
	close(release)
	<-done
	b := blockOf(t, store, "203.0.113.7/32")
	if b.BlockedUntil == 0 {
		t.Fatalf("%+v: the third denial was not counted before the reset, so three in a row did not block", b)
	}
	if b.ConsecutiveFailures != 0 {
		t.Fatalf("%+v: the sign-in's reset was lost or came first", b)
	}
	if n := auditCount(t, store, "remote_access_denied"); n != 3 {
		t.Fatalf("remote_access_denied rows %d, want 3: the held-back write counted twice or not at all", n)
	}
}
