package webserver

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// pairedNoiseOld is the estimator TestAdmission_RefusalTiming… used until the
// CI failures of 2026-09-02: the distance between the medians of the even and
// the odd rounds. Kept here only so the cell below can say, with numbers, what
// it did on the same synthetic rounds. It is not used by any assertion.
func pairedNoiseOld(d []time.Duration) time.Duration {
	var even, odd []time.Duration
	for i, v := range d {
		if i%2 == 0 {
			even = append(even, v)
		} else {
			odd = append(odd, v)
		}
	}
	if len(even) == 0 || len(odd) == 0 {
		return 0
	}
	return abs(median(even) - median(odd))
}

// syntheticRound draws one request duration the way a loaded CI runner
// produces them: a slow base, log-normal jitter, and a GC pause on a fraction
// of attempts — argon2 allocates 64 MiB per login, so under the race detector
// pauses of tens of milliseconds land on random samples.
func syntheticRound(r *rand.Rand, base time.Duration, offset time.Duration) time.Duration {
	jitter := math.Exp(r.NormFloat64() * 0.08) // ±8% log-normal
	d := time.Duration(float64(base) * jitter)
	if r.Float64() < 0.3 {
		d += 20*time.Millisecond + time.Duration(r.Float64()*40)*time.Millisecond // 20–60ms pause
	}
	return d + offset
}

// The estimator must do two things on the rounds the real test produces: cover
// the null — when the two causes are the same distribution, the bound must
// contain the observed median difference essentially always — and see an
// offset the size the controls inject (the bound plus the control margin). Synthetic, seeded, heavy-tailed, 24
// rounds like the real test. The old estimator's coverage is logged, not
// asserted: it is the reason this cell exists, not something it defends.
func TestPairedSpread_CoversTheNullAndSeesAnOffset(t *testing.T) {
	const runs, rounds = 3000, timingRounds
	r := rand.New(rand.NewPCG(20260902, 59))
	base := 130 * time.Millisecond // the CI runner's request, not VM43's 70ms

	var covered, coveredOld, seen int
	var minBound, maxBound time.Duration = time.Hour, 0
	for range runs {
		// Null: a and b drawn from the same distribution.
		d := make([]time.Duration, rounds)
		for i := range rounds {
			d[i] = syntheticRound(r, base, 0) - syntheticRound(r, base, 0)
		}
		bound := max(timingBoundMultiple*medianStdErr(d), timingFloor)
		minBound, maxBound = min(minBound, bound), max(maxBound, bound)
		if abs(median(d)) <= bound {
			covered++
		}
		if abs(median(d)) <= max(4*pairedNoiseOld(d), timingFloor) {
			coveredOld++
		}
		// Power, the way the committed controls exercise it: inject THIS run's
		// bound plus the control margin into one cause, resample, and compare
		// the new paired difference against the bound the first run accepted.
		o := make([]time.Duration, rounds)
		for i := range rounds {
			o[i] = syntheticRound(r, base, bound+timingControlMargin) - syntheticRound(r, base, 0)
		}
		if abs(median(o)) > bound {
			seen++
		}
	}
	t.Logf("null coverage: new %d/%d (%.3f%%), old even/odd %d/%d (%.3f%%); bound range %v–%v; bound+margin offset seen %d/%d",
		covered, runs, 100*float64(covered)/runs, coveredOld, runs, 100*float64(coveredOld)/runs, minBound, maxBound, seen, runs)
	if covered < runs-runs/1000 { // ≥ 99.9%
		t.Fatalf("null coverage %d/%d: the bound fails a same-distribution pair more than one run in a thousand — the test would flake on a quiet CI just as it did", covered, runs)
	}
	if seen < runs*99/100 {
		t.Fatalf("power %d/%d: an offset of the bound plus %v — what the controls inject — is missed more than one run in twenty", seen, runs, timingControlMargin)
	}
}
