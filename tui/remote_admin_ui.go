package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// The Manage dialog's server sections: Remote Control and Blocked IPs, an
// admin's, and Remote activity, anyone's (their own, unless an admin). Each
// is the connected server's.

// remoteControlHelp is what Remote Control does, beside its switch.
const remoteControlHelp = "Remote Control lets people use the autodb TUI on their own computer to reach this server " +
	"over an encrypted SSH connection. Nobody gets a login on this machine: the connection carries autodb only — " +
	"no shell, no psql, no port forwarding.\n\n" +
	"Only users who have an SSH public key registered on their autodb profile can connect. The first connection " +
	"from a computer enrolls it as that key's device; the same key from any other computer is refused. Every " +
	"connection asks for the user's autodb passphrase.\n\n" +
	"Logged: new devices, a device seen from a new IP address, and every refused connection with its IP. Three " +
	"refused attempts in a row block that IP for 24 hours.\n\n" +
	"The server must be reachable on port 7422 (open it in the cloud firewall). Turning this off disconnects " +
	"everyone connected remotely."

// ---- Remote Control ----------------------------------------------------------

// loadControl reads Remote Control into its section, then says done.
func (h *Host) loadControl(done string) {
	b := h.session.Bind()
	h.controlBound, h.controlLoaded = b, false
	h.set("App.controlText", "loading…")
	do(h, func(ctx context.Context) controlRead {
		st, err := b.RemoteControl(ctx)
		return controlRead{st, err}
	}, func(r controlRead) { h.showControl(b, r, done) })
}

type controlRead struct {
	st  RemoteControl
	err error
}

// controlCurrent reports whether the Remote Control section's pinned
// identity is still the session's.
func (h *Host) controlCurrent(b *Bound) bool {
	return b != nil && b == h.controlBound && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

// showControl shows what Remote Control is doing.
func (h *Host) showControl(b *Bound, r controlRead, done string) {
	if !h.controlCurrent(b) {
		return
	}
	if r.err != nil {
		h.set("App.controlText", "Remote Control: "+WireErrorMessage(r.err)+"\n\n"+remoteControlHelp)
		h.set("App.manageStatus", WireErrorMessage(r.err))
		return
	}
	h.controlState, h.controlLoaded = r.st, true
	h.set("App.controlText", controlText(r.st))
	toggle := "&Turn on"
	if r.st.On {
		toggle = "&Turn off"
	}
	h.set("App.controlToggleLabel", toggle)
	h.set("App.manageStatus", done)
}

// controlText is Remote Control's state, then its help.
func controlText(st RemoteControl) string {
	var lines []string
	switch {
	case !st.On:
		lines = append(lines, "Remote Control is OFF: no remote listener is open.")
	case st.State == "listening":
		lines = append(lines, "Remote Control is ON, listening on "+st.Addr+".")
	default:
		line := "Remote Control is ON, but the listener failed and is retrying"
		if st.Err != "" {
			line += ": " + st.Err
		}
		if st.NextTry != 0 {
			line += " (next try " + time.Unix(st.NextTry, 0).Format("15:04:05") + ")"
		}
		lines = append(lines, line+".")
	}
	if st.Paused != "" {
		lines = append(lines, "Remote connections paused — denials cannot be recorded ("+st.Paused+").")
	}
	if st.HostKeyFP != "" {
		lines = append(lines, "Host key: "+st.HostKeyFP+" — share it with the people who connect; "+
			"their first connect asks them to confirm it.")
	}
	if st.On {
		lines = append(lines, fmt.Sprintf("Live remote connections: %d.", st.Live))
	}
	return strings.Join(lines, "\n") + "\n\n" + remoteControlHelp
}

// controlToggle is App.controlToggle: turn Remote Control on, or, after
// saying what it ends, off. Off from a remote session ends this one too:
// leaveRemote.
func (h *Host) controlToggle() error {
	b := h.controlBound
	if !h.on(sectionControl) || !h.controlLoaded || !h.controlCurrent(b) {
		return nil
	}
	if !h.controlState.On {
		h.setControl(b, true, "Remote Control is on — share the host key with the people who connect")
		return nil
	}
	text := fmt.Sprintf("The remote listener closes and every remote connection ends now (%d live). "+
		"Nobody can connect remotely until it is turned on again.", h.controlState.Live)
	remote := h.session.Remote()
	if remote {
		text += "\n\nYou are connected remotely: your own session ends too, and you are back on the local daemon. " +
			"Turning it on again needs someone on the server host."
	}
	h.confirm("turn Remote Control off?", text, "&Turn off", "&Keep", func() {
		if !remote {
			h.setControl(b, false, "Remote Control is off")
			return
		}
		profile, _ := h.session.RemoteProfile()
		h.leaveRemote(b, "turn Remote Control off", func(ctx context.Context, b *Bound) error {
			_, err := b.SetRemoteControl(ctx, false)
			return err
		}, func(err error) string {
			return "turned Remote Control off on " + profileLabel(profile) + " — back on the local daemon"
		})
	})
	return nil
}

// setControl turns Remote Control on or off from a session it does not end.
func (h *Host) setControl(b *Bound, on bool, done string) {
	h.set("App.manageStatus", "…")
	do(h, func(ctx context.Context) controlRead {
		st, err := b.SetRemoteControl(ctx, on)
		return controlRead{st, err}
	}, func(r controlRead) { h.showControl(b, r, done) })
}

// ---- Blocked IPs -------------------------------------------------------------

func newBlocksManager() *manager[BlockRow] {
	return newManager("App.manageStatus",
		func(ctx context.Context, b *Bound) ([]BlockRow, error) { return b.Blocks(ctx) },
		func(r BlockRow) tuidecl.Row {
			state := "counting"
			if r.Blocked {
				state = "blocked, " + timeLeft(r.BlockedUntil) + " left"
			}
			return tuidecl.Row{"key": r.Prefix, "prefix": r.Prefix, "failures": r.Failures,
				"state": state, "last": shortDate(r.LastFailureAt), "reason": r.Reason}
		}, "key", "prefix", "failures", "state", "last", "reason")
}

// timeLeft is how long until unix, in hours and minutes.
func timeLeft(unix int64) string {
	d := time.Until(time.Unix(unix, 0)).Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// blockUnblock is App.blockUnblock(i): lift a prefix's block and clear its
// count.
func (h *Host) blockUnblock(i int) error {
	if !h.on(sectionBlocks) {
		return nil
	}
	r, ok := h.blocks.at(i)
	if !ok {
		h.set("App.manageStatus", "choose an address first")
		return nil
	}
	managerCall(h, h.blocks, "unblock "+r.Prefix, func(ctx context.Context, b *Bound) error {
		return b.Unblock(ctx, r.Prefix)
	})
	return nil
}

// ---- Remote activity ---------------------------------------------------------

// activityKind is one choice of the activity section's kind filter.
type activityKind struct{ label, action string }

var activityKindsAll = []activityKind{
	{"every kind", ""},
	{"devices enrolled", "remote_device_enrolled"},
	{"new IP addresses", "remote_new_ip"},
	{"refused connections", "remote_access_denied"},
}

// activityKindsFor are the kinds offered: refusals are an admin's.
func activityKindsFor(admin bool) []activityKind {
	if admin {
		return activityKindsAll
	}
	return activityKindsAll[:3]
}

func newActivityManager() *manager[AuditRow] {
	return newManager("App.manageStatus",
		func(context.Context, *Bound) ([]AuditRow, error) { return nil, nil }, // set per reload
		func(r AuditRow) tuidecl.Row {
			return tuidecl.Row{"when": r.CreatedAt, "who": r.User, "action": activityLabel(r.Action),
				"ip": r.IP, "detail": strings.ReplaceAll(r.Detail, "\n", "␤")}
		}, "when", "who", "action", "ip", "detail")
}

func activityLabel(action string) string {
	for _, k := range activityKindsAll[1:] {
		if k.action == action {
			return k.label
		}
	}
	return action
}

// activityNav is the activity section's kind and pages.
type activityNav struct {
	kinds []activityKind
	kind  int
	pages []*AuditCursor
	next  *AuditCursor
}

// openActivity loads the activity section's first page, of every kind.
func (h *Host) openActivity() {
	kinds := activityKindsFor(h.session.IsAdmin())
	h.activityNav = activityNav{kinds: kinds, pages: []*AuditCursor{nil}}
	rows := make([]tuidecl.Row, len(kinds))
	for i, k := range kinds {
		rows[i] = tuidecl.Row{"key": k.label, "label": k.label}
	}
	h.activityKinds.Reset(rows)
	h.set("App.activityKindIndex", -1)
	h.set("App.activityKindIndex", 0)
	h.activity.all, h.activity.rows = nil, nil
	h.activity.model.Reset(nil)
	h.activity.bound = h.session.Bind()
	h.activityReload()
}

// activityReload loads the current page.
func (h *Host) activityReload() {
	q := AuditQuery{Limit: historyPageSize, Before: h.activityNav.pages[len(h.activityNav.pages)-1]}
	if a := h.activityNav.kinds[h.activityNav.kind].action; a != "" {
		q.Actions = []string{a}
	}
	got := &AuditPage{}
	h.activity.load = func(ctx context.Context, b *Bound) ([]AuditRow, error) {
		p, err := b.SearchRemoteActivity(ctx, q)
		*got = p
		return p.Rows, err
	}
	h.activity.after = func() { h.activityNav.next = got.Next }
	h.activityNav.next = nil
	line := "n/p page"
	if n := len(h.activityNav.pages); n > 1 {
		line += fmt.Sprintf(" · page %d", n)
	}
	reloadManager(h, h.activity, line)
}

// activityKindChosen is App.activityKind(i): another kind, from the first
// page.
func (h *Host) activityKindChosen(i int) error {
	if !h.on(sectionActivity) || h.activity.bound == nil || i < 0 || i >= len(h.activityNav.kinds) {
		return nil
	}
	h.activityNav.kind = i
	h.activityNav.pages = []*AuditCursor{nil}
	h.activityReload()
	return nil
}

// activityNext is App.activityNext: the next page, when there is one.
func (h *Host) activityNext() error {
	if !h.on(sectionActivity) || h.activity.bound == nil {
		return nil
	}
	if h.activityNav.next == nil {
		h.set("App.manageStatus", "this is the last page")
		return nil
	}
	h.activityNav.pages = append(h.activityNav.pages, h.activityNav.next)
	h.activityReload()
	return nil
}

// activityPrev is App.activityPrev: the page before.
func (h *Host) activityPrev() error {
	if !h.on(sectionActivity) || h.activity.bound == nil {
		return nil
	}
	if len(h.activityNav.pages) == 1 {
		h.set("App.manageStatus", "this is the first page")
		return nil
	}
	h.activityNav.pages = h.activityNav.pages[:len(h.activityNav.pages)-1]
	h.activityReload()
	return nil
}
