package pressure

import "strconv"

// Signal names. Stable, because they are the identity a raise and its clear
// pair up on and the string an operator will end up grepping for at three in
// the morning.
const (
	SessionsGlobal   = "sessions.global"
	SessionsUser     = "sessions.user"
	LeasesTarget     = "leases.target"
	DenialsRate      = "denials.rate"
	SourcesThrottled = "sources.throttled"
)

// Caps is the occupancy state a pressure tick reads, supplied by the caller
// from figures that already exist.
type Caps struct {
	Sessions, SessionCap int
	PerUser              map[int64]int
	PerUserCap           int
	Leases               map[int64]int
	LeaseCap             int
}

// Readings converts one tick's observed state into the readings a Tracker
// judges.
//
// CONVERSION ONLY. Nothing here decides whether anything crossed, and nothing
// here remembers. Given the same inputs it returns the same readings, which is
// what lets the threshold rules be tested without a front door and the front
// door be tested without re-deriving the rules.
//
// ZERO-CAP DIMENSIONS ARE OMITTED ENTIRELY rather than emitted with a cap of
// zero. "No limit configured" is not a figure that can be a fraction of
// anything, and a signal that can never raise is one more row an operator has
// to learn to skip.
//
//	[Observed State Inputs]
//	• Caps (Sessions, PerUser, Leases)
//	• Denials (moving window count)
//	• Throttled (remote source addresses)
//	              │
//	              ▼
//	      [Readings(c, denials, throttled)]
//	              │
//	              ├─ SessionCap > 0 ───> Signal{sessions.global, Capacity, Occupancy}
//	              ├─ PerUserCap > 0 ───> Signal{sessions.user{id}, Capacity, Occupancy}
//	              ├─ LeaseCap > 0   ───> Signal{leases.target{id}, Capacity, Occupancy}
//	              ├─ Denials        ───> Signal{denials.rate{capacity}, Capacity, Rate}
//	              └─ Throttled      ───> Signal{sources.throttled{src}, Credential, Count}
func Readings(c Caps, denials int, throttled []string) []Reading {
	var out []Reading

	if c.SessionCap > 0 {
		out = append(out, Reading{
			Signal: Signal{ID: ID{Name: SessionsGlobal}, Class: Capacity, Kind: Occupancy},
			Value:  c.Sessions, Cap: c.SessionCap,
		})
	}
	if c.PerUserCap > 0 {
		for user, n := range c.PerUser {
			out = append(out, Reading{
				Signal: Signal{
					ID:    ID{Name: SessionsUser, Subject: strconv.FormatInt(user, 10)},
					Class: Capacity, Kind: Occupancy,
				},
				Value: n, Cap: c.PerUserCap,
			})
		}
	}
	if c.LeaseCap > 0 {
		for target, n := range c.Leases {
			out = append(out, Reading{
				Signal: Signal{
					ID:    ID{Name: LeasesTarget, Subject: strconv.FormatInt(target, 10)},
					Class: Capacity, Kind: Occupancy,
				},
				Value: n, Cap: c.LeaseCap,
			})
		}
	}

	// The denial signal is emitted at zero as well as above it.
	//
	// I CHECKED WHETHER THIS IS LOAD-BEARING AND IT IS NOT, so the comment says
	// what is true rather than what sounds convincing. An earlier version
	// claimed a rate omitted at zero could never be seen to clear; it can, by
	// the tracker's own path for a signal whose subject went away, and that
	// clear is indistinguishable from this one -- same identity, same zero
	// value, same zero threshold. I found that by omitting it and watching the
	// cell still pass.
	//
	// It stays because a rate is a property of the target, not of a subject
	// that comes and goes: emitting it always keeps the reading set the same
	// shape on every tick, which is easier to reason about than a row that
	// appears and disappears. That is a preference with a reason, not a
	// guarantee, and it is written here as the former.
	out = append(out, Reading{
		Signal: Signal{ID: ID{Name: DenialsRate, Subject: "capacity"},
			Class: Capacity, Kind: Rate},
		Value: denials,
	})

	// A THROTTLED SOURCE IS CREDENTIAL, NEVER CAPACITY. Reporting it as capacity
	// would tell an operator to resize a pool over somebody guessing passwords,
	// which is the incident's mistake wearing operations clothing.
	for _, src := range throttled {
		out = append(out, Reading{
			Signal: Signal{ID: ID{Name: SourcesThrottled, Subject: src},
				Class: Credential, Kind: Count},
			Value: 1,
		})
	}
	return out
}
