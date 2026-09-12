package tui

import (
	"context"
	"fmt"
	"strings"
)

// The SERVICE KEYSLOT's operator surface.
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

// showKeyslot displays the current unattended unlock status and actions in a floating panel.
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

	// THE HEADLINE IS WHAT IS TRUE NOW, and the boot probe is reported as
	// history beneath it.
	//
	// A review of the field report found the two collapsed: the status came
	// only from the boot probe, so an operator who cut a slot from this very
	// modal was still told "no service keyfile" and reasonably concluded the
	// enrolment had silently failed. It had not. What was stale was the
	// reading, and the fix is to say WHEN each claim was taken.
	if st.Checked && st.Verified && !st.Unlocked {
		p("UNATTENDED UNLOCK: ENROLLED AND VERIFIED")
		p("")
		p("The service keyslot was proven to open this store%s.", atClause(st.VerifiedAt))
		p("It did NOT open it at start — see below — so this daemon is running")
		p("on a passphrase login. The NEXT restart will unlock unattended.")
		p("")
		p("A verification is not a promise about the future: the keyfile can")
		p("still be deleted, re-moded or replaced after this check.")
		p("")
		p("At daemon start:")
		p("  %s", firstLine(st.Reason))
		return finishKeyslotText(&b, p, st)
	}
	// REMOVED is an assertive claim, so it requires a KNOWN absence. A review
	// found this branch rendering an unanswered lookup as a deliberate
	// removal: SlotPresent was derived from a query that mapped every failure
	// to false, so a database hiccup during the boot probe told an operator
	// somebody had deleted their keyslot.
	if st.Checked && !st.Verified && !st.SlotPresent && st.SlotPresentKnown && st.Attempted {
		// A deliberate removal. The boot record still says what happened at
		// start, which remains true, and this does NOT claim the running
		// process relocked — it holds the key it already unwrapped.
		p("UNATTENDED UNLOCK: REMOVED")
		p("")
		p("The service keyslot was deleted%s, so the NEXT restart will need a", atClause(st.VerifiedAt))
		p("passphrase login. This process still holds the key it already")
		p("unwrapped, so work in flight is unaffected.")
		return finishKeyslotText(&b, p, st)
	}
	if st.Checked && !st.SlotPresentKnown && st.VerifyReason != "" && !st.Unlocked {
		// The store could not be asked. Reported as ignorance, because the
		// alternative is inventing an answer.
		p("UNATTENDED UNLOCK: CANNOT BE DETERMINED")
		p("")
		p("The keyslot could not be inspected%s:", atClause(st.VerifiedAt))
		p("")
		p("  %s", firstLine(st.VerifyReason))
		p("")
		p("This says nothing about whether a slot exists -- only that the store")
		p("could not be read. Do not re-enroll on the strength of this screen.")
		return finishKeyslotText(&b, p, st)
	}
	if st.Checked && !st.Verified && st.SlotPresent && st.VerifyReason != "" && !st.Unlocked {
		// Committed and unverified: a real state, and the one an operator
		// most needs named rather than folded into either success or failure.
		p("UNATTENDED UNLOCK: CUT BUT NOT WORKING")
		p("")
		p("A service keyslot exists and it does NOT open this store:")
		p("")
		p("  %s", firstLine(st.VerifyReason))
		p("")
		p("Nothing was re-cut or removed, because that would strand whichever")
		p("half is still good. The next restart leaves the store locked.")
		return finishKeyslotText(&b, p, st)
	}

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
	return finishKeyslotText(&b, p, st)
}

// finishKeyslotText appends the one claim that is about NOW rather than about
// the keyslot, shared by every branch above so no branch can forget it.
func finishKeyslotText(b *strings.Builder, p func(string, ...any), st KeyslotStatus) string {
	p("")
	if st.StoreUnlocked {
		p("The store is UNLOCKED right now, so work is proceeding normally.")
	} else {
		p("The store is LOCKED right now: every connection needing a stored")
		p("secret is refused until somebody logs in with a passphrase.")
	}
	return b.String()
}

// atClause dates a claim, because "was proven" without a when is the ambiguity
// this whole change is about. Empty when the daemon did not report a time,
// rather than inventing one.
func atClause(ts string) string {
	if strings.TrimSpace(ts) == "" {
		return ""
	}
	return " at " + ts
}

// firstLine keeps a multi-line error from pushing the rest of the card off
// screen; the full text is in the daemon log.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if strings.TrimSpace(s) == "" {
		return "(no reason recorded)"
	}
	return s
}

// confirmEnrollKeyslot prompts confirmation before cutting and enrolling the unattended service keyslot.
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

// confirmRemoveKeyslot prompts confirmation before removing the unattended service keyslot.
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
