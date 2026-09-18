package tui

// THE LEADER MENU MUST NOT HAVE MOVED.
//
// leaderEntries became a projection of the command catalog rather than a
// literal list. That is an internal refactor and the operator must not be
// able to tell: same keys, same labels, same order, in every availability
// state. This file is the oracle for that claim.
//
// ONE DELTA IS EXPECTED AND ASSERTED: `u — users…` used to be offered to
// everyone while core/auth.ListUsers opens with requireAdmin, so for an editor
// or reader it could only fail. It is admin-only now. That is a defect fix, it
// is the ONLY permitted difference, and the cells below would fail if any other
// row moved.

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// leaderState is the five axes the projection reads.
type leaderState struct {
	role      string
	frontend  Frontend
	connected bool
	canSpawn  bool
	banner    bool
}

// modelFor builds the smallest Model the leader projection needs.
//
// Unexported fields are set directly because this is an in-package test and the
// alternative — standing up a real server per cell — would make a matrix of
// twenty states cost minutes and hide the thing under test behind transport.
func leaderModelFor(t *testing.T, st leaderState) *Model {
	t.Helper()
	var spawn func() (string, error)
	if st.canSpawn {
		spawn = func() (string, error) { return "", nil }
	}
	s := NewSession("", nil, spawn)
	s.user = UserInfo{ID: 1, Name: "u", Role: st.role}
	if st.connected {
		// Connected() only asks whether a client exists. A zero client is
		// enough to answer that, and nothing in the projection calls through it.
		s.client = &golibrpc.Client{}
	}
	m := &Model{session: s, frontend: st.frontend}
	m.cleartextFD = st.banner
	cat, err := NewCatalog(commandCatalog(), menuNodes())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	m.catalog = cat
	return m
}

// rows renders the projection as "key label" lines, which is the whole of what
// the operator sees and therefore the whole of what parity means.
func rows(m *Model) []string {
	out := []string{}
	for _, e := range m.leaderEntries() {
		out = append(out, string(e.key)+" "+e.label)
	}
	return out
}

// baseRows is every row an admin terminal sees with no banner and no spawner,
// in order. Transcribed from the pre-refactor leaderEntries.
func baseRows(connLabel string) []string {
	return []string{
		"r run query (selection when active)",
		"R run selection only",
		"j toggle results table/JSON",
		"z zoom focused pane (also Ctrl-w z)",
		"e focus explorer",
		"q focus query editor",
		"t focus results",
		"n new note",
		"s save note",
		"C select the query connection",
		"c connections…",
		"w workspaces…",
		"u users…",
		"i my allowed IPs…",
		"T my access tokens…",
		"H script history…",
		"k front-door CA certificate…",
		"g refresh explorer",
		"I ip allowlist (admin)…",
		"K service keyslot (admin)…",
		"L login / switch user",
		"x " + connLabel,
		// Added with the Profile surface. Declared here for the same reason
		// the pressure entry is: the menu is pinned so an addition has to be
		// stated rather than appearing quietly.
		"o profile",
		"A about autodb",
		// Added with the pressure view. The menu is pinned so an addition has to
		// be declared here rather than appearing quietly -- which is the point of
		// pinning it, and is how this entry was noticed.
		"P front-door pressure",
		"? help",
		"Q quit",
	}
}

// without drops rows whose key is in keys, preserving order.
func without(in []string, keys ...string) []string {
	out := []string{}
	for _, r := range in {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(r, k+" ") {
				drop = true
			}
		}
		if !drop {
			out = append(out, r)
		}
	}
	return out
}

// insertAfter puts row directly after the row with key `after`.
func insertAfter(in []string, after, row string) []string {
	out := []string{}
	for _, r := range in {
		out = append(out, r)
		if strings.HasPrefix(r, after+" ") {
			out = append(out, row)
		}
	}
	return out
}

// TestTheLeaderMenuIsUnchangedExceptForTheAdminOnlyUsersEntry.
func TestTheLeaderMenuIsUnchangedExceptForTheAdminOnlyUsersEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state leaderState
		want  []string
	}{
		{
			name:  "admin terminal, disconnected, no spawner, no banner",
			state: leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal},
			want:  baseRows("connect"),
		},
		{
			name: "admin terminal, connected",
			state: leaderState{
				role: meta.RoleAdmin, frontend: FrontendTerminal, connected: true,
			},
			want: baseRows("disconnect"),
		},
		{
			name: "admin terminal with a spawner offers restart, after x",
			state: leaderState{
				role: meta.RoleAdmin, frontend: FrontendTerminal, canSpawn: true,
			},
			want: insertAfter(baseRows("connect"), "x", "X restart the server"),
		},
		{
			name: "the no-TLS warning adds ! after the admin surfaces",
			state: leaderState{
				role: meta.RoleAdmin, frontend: FrontendTerminal, banner: true,
			},
			// After K, not after g: the original appends the banner entry once
			// the admin block has been added.
			want: insertAfter(baseRows("connect"), "K", "! dismiss the no-TLS warning"),
		},
		{
			// THE DELTA. Everything else is byte-identical to the admin case.
			name:  "editor terminal loses u, I and K",
			state: leaderState{role: meta.RoleEditor, frontend: FrontendTerminal},
			want:  without(baseRows("connect"), "u", "I", "K"),
		},
		{
			name:  "reader terminal loses the same three",
			state: leaderState{role: meta.RoleReader, frontend: FrontendTerminal},
			want:  without(baseRows("connect"), "u", "I", "K"),
		},
		{
			// An unknown role — the pre-login state — keeps every AudienceAll
			// command and loses only the admin ones.
			name:  "unknown role keeps the base list and loses only the admin three",
			state: leaderState{role: "", frontend: FrontendTerminal},
			want:  without(baseRows("connect"), "u", "I", "K"),
		},
		{
			// The web frontend does not own its session: no login, no
			// connection toggle, no restart. Q SURVIVES — it ends that tab and
			// is valid everywhere.
			name:  "web admin loses L and x but keeps Q",
			state: leaderState{role: meta.RoleAdmin, frontend: FrontendWeb},
			want:  without(baseRows("connect"), "L", "x"),
		},
		{
			name:  "web editor loses L, x, u, I and K",
			state: leaderState{role: meta.RoleEditor, frontend: FrontendWeb},
			want:  without(baseRows("connect"), "L", "x", "u", "I", "K"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rows(leaderModelFor(t, tc.state))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d\n got: %v\nwant: %v",
					len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestQuitIsOfferedInEveryState.
//
// Called out on its own because an earlier draft of the ADR claimed the web
// frontend withdrew Exit. It does not — `Q` is appended outside the
// managesOwnAuth block — and the claim survived into a review. A specific cell
// is cheaper than remembering.
func TestQuitIsOfferedInEveryState(t *testing.T) {
	for _, role := range []string{meta.RoleAdmin, meta.RoleEditor, meta.RoleReader, ""} {
		for _, fe := range []Frontend{FrontendTerminal, FrontendWeb} {
			for _, connected := range []bool{false, true} {
				for _, banner := range []bool{false, true} {
					m := leaderModelFor(t, leaderState{
						role: role, frontend: fe, connected: connected, banner: banner,
					})
					found := false
					for _, e := range m.leaderEntries() {
						if e.key == 'Q' {
							found = true
						}
					}
					if !found {
						t.Errorf("Q is missing for role=%q frontend=%v connected=%v banner=%v",
							role, fe, connected, banner)
					}
				}
			}
		}
	}
}

// TestEveryOfferedLeaderRowCanActuallyRun.
//
// The rule this menu has always had: an entry that can only fail teaches
// distrust of the menu. The projection's job is to keep that true, so every row
// it emits must have a handler — and a disabled row must NOT, since a nil
// handler is how the which-key float knows to refuse the key and stay open.
func TestEveryOfferedLeaderRowCanActuallyRun(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	for _, e := range m.leaderEntries() {
		if e.run == nil {
			t.Errorf("row %q has no handler but is not marked disabled", e.label)
		}
	}
}
