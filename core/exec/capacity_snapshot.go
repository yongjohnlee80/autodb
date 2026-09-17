package exec

// CapacitySnapshot is what the caps and their occupants looked like at one
// instant, for anything that wants to report pressure.
//
// A READ, NOT A COUNTER. Every figure here already exists and is already
// maintained by the paths that enforce the caps; this copies them out. A
// pressure surface that kept its own tallies would be a second source of truth
// about capacity, and when two sources disagree the one people are looking at
// is the one that is wrong.
type CapacitySnapshot struct {
	// Sessions and SessionCap are the instance-wide session occupancy.
	Sessions, SessionCap int
	// PerUser is each user's session count, against PerUserCap.
	PerUser    map[int64]int
	PerUserCap int
	// Leases is each target's wire-lease count, against LeaseCap.
	Leases   map[int64]int
	LeaseCap int
}

// CapacitySnapshot reads every cap and its occupancy in ONE hold.
//
// THE FIGURES HAVE TO AGREE WITH EACH OTHER. Read separately, the global count
// can come from before an admission and a per-user count from after it, and the
// view then shows a per-user total exceeding a global one — which is impossible,
// and which an operator will read as the instrument being broken rather than as
// a sampling artefact. It is the same reason the refusal that says "nothing is
// coming" is decided under the lock that made it.
func (e *Engine) CapacitySnapshot() CapacitySnapshot {
	r := e.sessions
	if r == nil {
		return CapacitySnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	snap := CapacitySnapshot{
		Sessions:   len(r.byID),
		SessionCap: r.globalCap,
		PerUser:    make(map[int64]int, len(r.perUser)),
		PerUserCap: r.perUserCap,
		Leases:     make(map[int64]int, len(r.leases)),
		LeaseCap:   r.leaseCap,
	}
	// COPIED, NOT SHARED. Handing out the live maps would let a reader range
	// over them while an admission writes, which is a data race the caller
	// could not see and could not fix.
	for u, n := range r.perUser {
		snap.PerUser[u] = n
	}
	for c, n := range r.leases {
		snap.Leases[c] = n
	}
	return snap
}
