package tui

import (
	"context"
	"strings"
)

// The boot probe is history; a checked slot is what was proven SINCE start.
// These two timelines must not be collapsed into a single enabled flag.
func keyslotStatusText(st KeyslotStatus) string {
	var lines []string
	switch {
	case st.Checked && st.Verified && !st.Unlocked:
		lines = []string{"UNATTENDED UNLOCK: ENROLLED AND VERIFIED", "The service keyslot opened this store" + keyslotAt(st.VerifiedAt) + ".",
			"It did not unlock at start; the NEXT restart will use it.", "At daemon start: " + keyslotReason(st.Reason)}
	case st.Checked && !st.Verified && !st.SlotPresent && st.SlotPresentKnown && st.Attempted:
		lines = []string{"UNATTENDED UNLOCK: REMOVED", "The keyslot was removed" + keyslotAt(st.VerifiedAt) + ".",
			"The NEXT restart needs a passphrase. This process keeps the key it already unwrapped."}
	case st.Checked && !st.SlotPresentKnown && st.VerifyReason != "" && !st.Unlocked:
		lines = []string{"UNATTENDED UNLOCK: CANNOT BE DETERMINED", "The slot could not be inspected" + keyslotAt(st.VerifiedAt) + ".",
			keyslotReason(st.VerifyReason), "Do not re-enroll on the strength of this unknown result."}
	case st.Checked && !st.Verified && st.SlotPresent && st.VerifyReason != "" && !st.Unlocked:
		lines = []string{"UNATTENDED UNLOCK: CUT BUT NOT WORKING", "A slot exists but does not open the store:",
			keyslotReason(st.VerifyReason), "The next restart leaves the store locked."}
	case st.Unlocked:
		lines = []string{"UNATTENDED UNLOCK: ACTIVE", "The service keyslot unlocked the daemon at start."}
	case !st.Attempted:
		lines = []string{"UNATTENDED UNLOCK: NOT ENABLED", "No service keyslot was configured at daemon start.",
			"A restart requires a passphrase login until an operator enables it."}
	default:
		lines = []string{"UNATTENDED UNLOCK: FAILED", keyslotReason(st.Reason),
			"The daemon is running, but stored secrets may be unavailable."}
	}
	if st.StoreUnlocked {
		lines = append(lines, "", "The store is UNLOCKED right now; work can proceed.")
	} else {
		lines = append(lines, "", "The store is LOCKED right now; a passphrase login is required.")
	}
	return strings.Join(lines, "\n")
}

func keyslotAt(ts string) string {
	if strings.TrimSpace(ts) == "" {
		return ""
	}
	return " at " + ts
}

func keyslotReason(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if s == "" {
		return "(no reason recorded)"
	}
	return s
}

const keyslotEnrollQuestion = `Enabling unattended unlock changes WHERE THIS INSTALL'S SECRETS ARE PROTECTED.

WHAT YOU GET: after reboot the daemon can unlock itself without a human login.

WHAT IT COSTS: at-rest protection shifts from a passphrase absent on disk to
filesystem permissions and host security. Anyone who can read BOTH the keyfile
and meta store can read every secret. Keep the keyfile in its own 0600 directory,
never next to the meta store or in a backup that includes both.

Every user's passphrase still works. The slot authenticates nobody: tokens are
still checked on every call. Removing the slot deletes its keyfile and makes
the NEXT restart require a passphrase again.`

const keyslotRemoveQuestion = `Removing the service keyslot deletes the slot AND its keyfile.

After the next restart this daemon is LOCKED until a passphrase login. If
autodb is the only path to a production database, that is an outage.

This process remains unlocked; work in flight is unaffected until restart.`

func (h *Host) openKeyslot() {
	if !h.session.IsAdmin() {
		return
	}
	h.keyslotBound = h.session.Bind()
	h.keyslotSeq++
	h.set("App.keyslotText", "loading…")
	h.set("App.keyslotCanEnable", false)
	h.set("App.keyslotCanRemove", false)
	h.readKeyslot(h.keyslotBound, h.keyslotSeq, "")
	h.open("keyslot")
}

func (h *Host) keyslotClosed() error {
	h.keyslotBound, h.keyslotConfirmBound = nil, nil
	h.keyslotSeq++
	return nil
}

func (h *Host) readKeyslot(b *Bound, seq uint64, done string) {
	read := h.keyslotRead
	if read == nil {
		read = func(ctx context.Context, b *Bound) (KeyslotStatus, error) { return b.KeyslotStatus(ctx) }
	}
	type answer struct {
		st  KeyslotStatus
		err error
	}
	do(h, func(ctx context.Context) answer {
		st, err := read(ctx, b)
		return answer{st, err}
	}, func(a answer) {
		if seq != h.keyslotSeq || b != h.keyslotBound || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		if a.err != nil {
			h.set("App.keyslotText", "UNATTENDED UNLOCK: CANNOT BE DETERMINED\nStatus could not be read: "+WireErrorMessage(a.err))
			h.set("App.keyslotCanEnable", false)
			h.set("App.keyslotCanRemove", false)
			status := "status could not be read"
			if done != "" {
				status = done + "; current state unavailable"
			}
			h.set("App.keyslotStatus", status)
			return
		}
		h.keyslotState = a.st
		h.set("App.keyslotText", keyslotStatusText(a.st))
		h.set("App.keyslotCanEnable", a.st.StoreUnlocked && !a.st.Unlocked && (!a.st.Attempted || (a.st.SlotPresentKnown && !a.st.SlotPresent)))
		h.set("App.keyslotCanRemove", a.st.Unlocked || a.st.SlotPresent || (a.st.Attempted && !a.st.Checked))
		h.set("App.keyslotStatus", done)
	})
}

func (h *Host) keyslotEnable() error       { return h.confirmKeyslot("enable") }
func (h *Host) keyslotRemoveAction() error { return h.confirmKeyslot("remove") }

func (h *Host) confirmKeyslot(mode string) error {
	b := h.keyslotBound
	if b == nil || b.User().Role != "admin" || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
		return nil
	}
	h.keyslotConfirmBound, h.keyslotConfirmSeq, h.keyslotConfirmMode = b, h.keyslotSeq, mode
	if mode == "enable" {
		h.set("App.keyslotConfirmTitle", "enable unattended unlock?")
		h.set("App.keyslotQuestion", keyslotEnrollQuestion)
	} else {
		h.set("App.keyslotConfirmTitle", "remove service keyslot?")
		h.set("App.keyslotQuestion", keyslotRemoveQuestion)
	}
	h.open("keyslotConfirm")
	return nil
}

func (h *Host) keyslotConfirmCancelled() error { h.keyslotConfirmBound = nil; return nil }

func (h *Host) keyslotConfirmed() error {
	b, seq, mode := h.keyslotConfirmBound, h.keyslotConfirmSeq, h.keyslotConfirmMode
	h.keyslotConfirmBound = nil
	if b == nil || b != h.keyslotBound || seq != h.keyslotSeq || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
		return nil
	}
	act := h.keyslotEnroll
	if act == nil {
		act = func(ctx context.Context, b *Bound) error { return b.EnrollKeyslot(ctx) }
	}
	done := "service keyslot enabled — the next restart unlocks unattended"
	if mode == "remove" {
		act, done = h.keyslotRemove, "service keyslot removed — next restart needs a passphrase"
		if act == nil {
			act = func(ctx context.Context, b *Bound) error { return b.RemoveKeyslot(ctx) }
		}
	} else if mode != "enable" {
		return nil
	}
	h.set("App.keyslotStatus", "updating service keyslot…")
	do(h, func(ctx context.Context) error { return act(ctx, b) }, func(err error) {
		if b != h.keyslotBound || seq != h.keyslotSeq || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		if err != nil {
			done = "service keyslot: " + WireErrorMessage(err)
			h.setStatus(done)
		}
		h.readKeyslot(b, seq, done) // verification can fail AFTER the slot commits
	})
	return nil
}
