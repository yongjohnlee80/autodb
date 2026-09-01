package frontdoor

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Matrix §10: startup denials must be indistinguishable across causes —
// MEASURED, not asserted.
//
// The distinction is the whole point and it is worth being explicit about.
// Asserting that two branches emit the same bytes proves the bytes; it says
// nothing about how long each took to decide. A surface whose refusals are
// byte-identical but whose timing separates "no such user" from "wrong
// password" still answers the question, just in a different channel — and
// timing is the channel an attacker with a TCP route already has, for free,
// on every attempt.
//
// This harness exists NOW, alongside the shape, rather than being retrofitted
// when the auth chain lands: the causes it compares are the ones this slice
// can produce, and F0c/F0d add their causes to the same table. Building it
// later would mean building it against branches already written to whatever
// timing they happened to have.

// denialSample dials, drives one cause to its denial, and returns how long
// the server took to answer.
func denialSample(t *testing.T, addr string, drive func(t *testing.T, addr string) (*tls.Conn, time.Time)) time.Duration {
	t.Helper()
	conn, sent := drive(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("reading the denial: %v", err)
	}
	return time.Since(sent)
}

func TestDenialTiming_IndistinguishableAcrossCauses(t *testing.T) {
	// The throttle is raised out of the way: this harness takes many samples
	// from one address and the subject is the denial PATH's timing, not the
	// rate limiter's.
	_, _, addr := listenerWith(t, Options{AuthFailuresPerIP: unthrottled})

	// The causes this slice can produce. Each drives the connection to the
	// SAME uniform denial by a different route.
	causes := map[string]func(t *testing.T, addr string) (*tls.Conn, time.Time){
		"an unsupported protocol major": func(t *testing.T, addr string) (*tls.Conn, time.Time) {
			tc := tlsDial(t, addr)
			now := time.Now()
			_, _ = tc.Write(startupPacket(uint32(4)<<16|0, map[string]string{"user": "root"}))
			return tc, now
		},
		"a startup that reaches auth with no credential store": func(t *testing.T, addr string) (*tls.Conn, time.Time) {
			tc := tlsDial(t, addr)
			now := time.Now()
			_, _ = tc.Write(startupPacket(protocolVersion30, map[string]string{"user": "root", "database": "lm-prod"}))
			return tc, now
		},
		"a malformed post-TLS frame": func(t *testing.T, addr string) (*tls.Conn, time.Time) {
			tc := tlsDial(t, addr)
			now := time.Now()
			junk := make([]byte, 8)
			binary.BigEndian.PutUint32(junk[0:4], 8)
			binary.BigEndian.PutUint32(junk[4:8], 0xDEADBEEF)
			_, _ = tc.Write(junk)
			return tc, now
		},
	}

	const rounds = 25
	medians := map[string]time.Duration{}
	for name, drive := range causes {
		samples := make([]time.Duration, 0, rounds)
		for i := 0; i < rounds; i++ {
			samples = append(samples, denialSample(t, addr, drive))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		medians[name] = samples[len(samples)/2]
	}

	// Medians rather than means: one scheduler hiccup in a CI container
	// should not decide a security property. The bound is deliberately
	// generous — this is a leak detector, not a benchmark, and it must fail
	// on a branch that does real extra work (a database round trip, a hash)
	// while tolerating the noise of a shared runner.
	var lo, hi time.Duration
	var loName, hiName string
	for name, m := range medians {
		if lo == 0 || m < lo {
			lo, loName = m, name
		}
		if m > hi {
			hi, hiName = m, name
		}
	}
	const tolerance = 25 * time.Millisecond
	if hi-lo > tolerance {
		t.Errorf("startup denials are distinguishable BY TIMING: %q takes %s and %q takes %s "+
			"(spread %s > %s). The bytes are identical, so a caller cannot read the cause from "+
			"the reply — but they can read it from the clock, which is a channel any peer with "+
			"a TCP route already has on every attempt.\nmedians: %v",
			hiName, hi, loName, lo, hi-lo, tolerance, medians)
	}

	// POSITIVE CONTROL for the instrument — a real measurement, not
	// arithmetic on numbers this test invented.
	//
	// The first version of this added 200ms to a median and checked the sum
	// exceeded the tolerance, which is a statement about addition. It would
	// have passed with the sampler completely broken. The control has to
	// MEASURE a cause that really is slower, through the same path, or a
	// green here proves only that the harness can add.
	slowAddr := slowDenialListener(t, 150*time.Millisecond)
	var slowSamples []time.Duration
	for i := 0; i < 9; i++ {
		slowSamples = append(slowSamples, denialSample(t, slowAddr, causes["a startup that reaches auth with no credential store"]))
	}
	sort.Slice(slowSamples, func(i, j int) bool { return slowSamples[i] < slowSamples[j] })
	slowMedian := slowSamples[len(slowSamples)/2]
	if slowMedian-lo <= tolerance {
		t.Fatalf("the harness measured a deliberately-delayed listener at %s against a normal "+
			"%s — a %s difference it failed to resolve above a %s tolerance. It cannot detect "+
			"a real timing leak, so its green above means nothing",
			slowMedian, lo, slowMedian-lo, tolerance)
	}
}

// slowDenialListener is a listener with an injected delay on the denial path,
// used only to prove the timing harness can see one.
func slowDenialListener(t *testing.T, delay time.Duration) string {
	t.Helper()
	now := time.Now()
	c := issueChain(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cfg, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "autodb.example.com"), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open("127.0.0.1:0", cfg, Options{AuthFailuresPerIP: unthrottled, testDenialDelay: delay})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); l.Close() })
	return l.Addr().String()
}
