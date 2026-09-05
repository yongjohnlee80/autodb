package tui

import (
	"context"
	"fmt"
	"strings"
)

// The SERVICE KEYSLOT's operator surface (ADR-0087 §5, §6).
//
// The daemon prints its banner once, at start, to a terminal nobody may be
// watching. This is the surface that answers the same question LATER — at the
// moment developers start being refused — and the one that cuts the slot in
// the first place.

// keyslotProse is what an admin reads BEFORE enabling the unattended unlock.
//
// A raw literal on purpose: this is a screen of text, and building it from
// escaped fragments is how it acquires a stray newline nobody notices until it
// is in front of the person making a security decision.
//
// It leads with WHAT IT COSTS rather than what it does, because the benefit is
// already obvious to whoever went looking for this menu, and the cost is not.
const keyslotProse = `Enabling the unattended unlock changes WHERE THIS INSTALL'S
SECRETS ARE PROTECTED. Read this before you decide.

  WHAT YOU GET. After a reboot the daemon unlocks itself and
  developers keep working. Today a restart locks everybody out
  until a human logs in by hand — and if autodb is the only path
  to your production database, that is an outage.

  WHAT IT COSTS. At-rest protection stops being "a passphrase
  that exists nowhere on disk" and becomes "filesystem
  permissions and host security". ANYONE WHO CAN READ BOTH the
  keyfile AND the meta store has every secret in it.

  WHAT DOES NOT CHANGE. Every user's passphrase keeps working
  exactly as it does now — this is a slot added beside them, not
  a replacement. And unlocking the key authenticates NOBODY:
  authority is still a token, checked on every call.

  WHERE THE KEYFILE GOES. Its own directory, mode 0600, owned by
  the service user. NOT beside the meta store: those two are the
  halves of one envelope, and one careless backup of a directory
  holding both hands over everything.

You can undo this at any time — removing the slot deletes the
keyfile too, and the next restart asks for a passphrase again.
`

// openKeyslotMenu is the operator's whole keyslot surface: what the state is,
// and the two mutations.
func (m *Model) openKeyslotMenu() {
	bound := m.session.Bind()
	m.ctx.Go(func(c context.Context) (any, error) {
		st, err := bound.KeyslotStatus(c)
		if err != nil {
			msg := WireErrorMessage(err)
			return managerReload{gen: bound.Gen(), apply: func() {
				m.setError("service keyslot: " + msg)
			}}, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() { m.showKeyslot(st) }}, nil
	})
}

func (m *Model) showKeyslot(st KeyslotStatus) {
	m.openTextFloat("service keyslot", keyslotStatusText(st))
	entries := []leaderEntry{
		{'e', "enable the unattended unlock (cut the slot)", m.confirmEnrollKeyslot},
	}
	// Removal is offered only when there is something to remove — this menu's
	// own rule that an entry which always fails teaches distrust of the menu.
	if st.Attempted || st.Unlocked {
		entries = append(entries,
			leaderEntry{'x', "disable it (delete the slot AND the keyfile)", m.confirmRemoveKeyslot})
	}
	m.openLeader("service keyslot", entries)
}

// keyslotStatusText answers the question an operator actually has, which is
// never "what is the flag" but "why is nobody able to connect".
//
// Separate from the rendering so its CONTENT is testable — a status screen
// asserted only by "it printed something" is one whose worst version passes.
func keyslotStatusText(st KeyslotStatus) string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	switch {
	case st.Unlocked:
		p("UNATTENDED UNLOCK: ACTIVE")
		p("")
		p("This daemon unlocked itself at start from the service keyslot.")
		p("A restart will NOT lock anybody out.")
	case !st.Attempted:
		p("UNATTENDED UNLOCK: NOT ENABLED")
		p("")
		p("This install has no service keyslot, so a restart locks the store")
		p("until somebody logs in with a passphrase. That is the default, and")
		p("it is the right one unless autodb is the only path to your database.")
	default:
		// THE CASE THIS SURFACE EXISTS FOR. The daemon stayed up, which is
		// correct, and the state is otherwise invisible to anyone who was not
		// watching the terminal at boot.
		p("UNATTENDED UNLOCK: FAILED")
		p("")
		p("  %s", st.Reason)
		p("")
		p("The daemon is RUNNING and answering. Front-door clients are being")
		p("refused with 57P03 \"the server is not accepting connections\" —")
		p("NOT an authentication failure, so nobody should be regenerating")
		p("tokens over this.")
	}
	p("")
	if st.StoreUnlocked {
		p("The store is UNLOCKED right now, so work is proceeding normally.")
	} else {
		p("The store is LOCKED right now: every connection needing a stored")
		p("secret is refused until somebody logs in with a passphrase.")
	}
	return b.String()
}

func (m *Model) confirmEnrollKeyslot() {
	m.openTextFloat("enable the unattended unlock?", keyslotProse)
	m.openLeader("enable the unattended unlock?", []leaderEntry{
		{'y', "yes — cut the slot and write the keyfile", func() {
			bound := m.session.Bind()
			m.ctx.Go(func(c context.Context) (any, error) {
				err := bound.EnrollKeyslot(c)
				return managerReload{gen: bound.Gen(), apply: func() {
					if err != nil {
						m.setError("service keyslot: " + WireErrorMessage(err))
						return
					}
					m.setOK("service keyslot cut — this daemon will unlock itself after a restart")
				}}, nil
			})
		}},
	})
}

func (m *Model) confirmRemoveKeyslot() {
	m.openTextFloat("disable the unattended unlock?",
		"Removing the service keyslot deletes the slot AND its keyfile.\n\n"+
			"After the next restart this daemon will be LOCKED until somebody\n"+
			"logs in with a passphrase — which is an outage if autodb is the\n"+
			"only path to your database.\n\n"+
			"This process stays unlocked; nothing breaks until the next restart.\n")
	m.openLeader("disable the unattended unlock?", []leaderEntry{
		{'y', "yes — delete the slot and the keyfile", func() {
			bound := m.session.Bind()
			m.ctx.Go(func(c context.Context) (any, error) {
				err := bound.RemoveKeyslot(c)
				return managerReload{gen: bound.Gen(), apply: func() {
					if err != nil {
						m.setError("service keyslot: " + WireErrorMessage(err))
						return
					}
					m.setOK("service keyslot removed — the next restart will need a passphrase")
				}}, nil
			})
		}},
	})
}
