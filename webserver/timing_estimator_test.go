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
	return syntheticRoundOn(r, quietMachine, base, offset)
}

// machineLoad shapes the noise of one sample: the jitter's spread, how often a
// pause lands, and how long one lasts.
type machineLoad struct {
	jitter     float64
	pauseOdds  float64
	pauseFloor time.Duration
	pauseSpan  time.Duration
}

var (
	quietMachine = machineLoad{jitter: 0.08, pauseOdds: 0.3, pauseFloor: 20 * time.Millisecond, pauseSpan: 40 * time.Millisecond}
	// busyMachine is a control sample taken on a busier moment than the bound's:
	// twice the jitter, pauses more often and up to 120ms.
	busyMachine = machineLoad{jitter: 0.16, pauseOdds: 0.5, pauseFloor: 20 * time.Millisecond, pauseSpan: 100 * time.Millisecond}
)

func syntheticRoundOn(r *rand.Rand, m machineLoad, base time.Duration, offset time.Duration) time.Duration {
	jitter := math.Exp(r.NormFloat64() * m.jitter) // log-normal
	d := time.Duration(float64(base) * jitter)
	if r.Float64() < m.pauseOdds {
		d += m.pauseFloor + time.Duration(r.Float64()*float64(m.pauseSpan))
	}
	return d + offset
}

// The estimator must do two things on the rounds the real test produces: cover
// the null — when the two causes are the same distribution, the bound must
// contain the observed median difference essentially always — and see an
// offset the size the controls inject (timingControlDelay), including when the
// control's sample lands on a busier moment than the bound's. Synthetic,
// seeded, heavy-tailed, 24 rounds like the real test. The old estimator's coverage is logged, not
// asserted: it is the reason this cell exists, not something it defends.
func TestPairedSpread_CoversTheNullAndSeesAnOffset(t *testing.T) {
	const runs, rounds = 3000, timingRounds
	r := rand.New(rand.NewPCG(20260902, 59))
	base := 130 * time.Millisecond // the CI runner's request, not VM43's 70ms

	var covered, coveredOld, seen, seenBusy int
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
		// control delay into one cause, resample, and compare the new paired
		// difference against the bound the first run accepted.
		delay := timingControlDelay(bound)
		o := make([]time.Duration, rounds)
		busy := make([]time.Duration, rounds)
		for i := range rounds {
			o[i] = syntheticRound(r, base, delay) - syntheticRound(r, base, 0)
			busy[i] = syntheticRoundOn(r, busyMachine, base, delay) - syntheticRoundOn(r, busyMachine, base, 0)
		}
		if abs(median(o)) > bound {
			seen++
		}
		if abs(median(busy)) > bound {
			seenBusy++
		}
	}
	t.Logf("null coverage: new %d/%d (%.3f%%), old even/odd %d/%d (%.3f%%); bound range %v–%v; control delay seen %d/%d, on a busier control sample %d/%d",
		covered, runs, 100*float64(covered)/runs, coveredOld, runs, 100*float64(coveredOld)/runs, minBound, maxBound, seen, runs, seenBusy, runs)
	if covered < runs-runs/1000 { // ≥ 99.9%
		t.Fatalf("null coverage %d/%d: the bound fails a same-distribution pair more than one run in a thousand — the test would flake on a quiet CI just as it did", covered, runs)
	}
	if seen < runs*999/1000 {
		t.Fatalf("power %d/%d: the delay the controls inject is missed more than one run in a thousand", seen, runs)
	}
	if seenBusy < runs*97/100 {
		t.Fatalf("power on a busier control sample %d/%d: the delay the controls inject is missed more than three runs in a hundred", seenBusy, runs)
	}
}
