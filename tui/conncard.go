package tui

import (
	"fmt"
	"iter"
	"net"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// The connection card (ADR-0086 §8): what replaced a float holding nothing but
// the secret.
//
// The old reveal showed a token and left the person to find out for themselves
// which host, which port, which database name, and which sslmode. Every one of
// those was a separate failure the first time a real GUI client was pointed at
// this surface, and the database NAME in particular cost an hour.
//
// It is READ-ONLY BUT INTERACTIVE — a focused Editor with SetReadOnly, the same
// contract as the script viewer — because a static dump cannot be navigated or
// yanked from, and this is the only time the secret exists anywhere.

// cardCopy names what a copy key yields. TWO keys, because the card's body now
// carries instructions: a single `y` over a screen of prose would paste a
// paragraph into a password field.
type cardCopy struct {
	key   rune
	label string
	value string
}

type connCard struct {
	widget.Base
	model  *Model
	text   string
	copies []cardCopy
	editor *widget.Editor
	ctx    *tui.Context
	float  *widget.Float
}

// buildCardDSN renders the FRONT-DOOR DSN — the one a client dials.
//
// user is the autodb account, password is the token, host and port are the
// front door's. It is NEVER the target's own DSN: plaintext target credentials
// do not leave the security core (security-core-hardening R8), and a card that
// printed them would turn a show-once credential reveal into a database
// password disclosure.
func buildCardDSN(host, port, user, secret, database, sslmode, rootCA string) string {
	q := "sslmode=" + sslmode
	if rootCA != "" {
		q += "&sslrootcert=" + rootCA
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?%s", user, secret, host, port, database, q)
}

// buildCardJDBC renders the JetBrains form, because JetBrains is the client
// that surfaced every defect this ADR fixes.
func buildCardJDBC(host, port, user, secret, database, sslmode, rootCA string) string {
	q := fmt.Sprintf("user=%s&password=%s&ssl=true&sslmode=%s", user, secret, sslmode)
	if rootCA != "" {
		q += "&sslrootcert=" + rootCA
	}
	return fmt.Sprintf("jdbc:postgresql://%s:%s/%s?%s", host, port, database, q)
}

// splitAddr separates a live bound address, tolerating an address that is not
// host:port rather than losing the whole card to it.
func splitAddr(addr string) (host, port string) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, ""
	}
	// A wildcard bind is not something a client can dial. Say so rather than
	// printing it: "0.0.0.0" in a Host field is the kind of instruction that
	// looks authoritative and fails.
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = ""
	}
	return h, p
}

func (c *connCard) AcceptsFocus() bool { return false }

func (c *connCard) Init(ctx *tui.Context) {
	c.Base.Init(ctx)
	c.ctx = ctx
	c.editor = widget.NewEditor(widget.WithEditorStyles(widget.TextInputStyles{
		Selection: cursorRowStyle,
	}))
	c.editor.SetValue(c.text)
	c.editor.SetReadOnly(true)
	ctx.Mount(c.editor)
	ctx.FocusComponent(c.editor)
}

func (c *connCard) Layout(cs tui.Constraints) tui.Size {
	w, h := cs.MaxW, cs.MaxH
	sz := c.ctx.LayoutChild(c.editor, tui.Tight(tui.Size{W: w, H: h}))
	c.ctx.PlaceChild(c.editor, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return cs.Constrain(tui.Size{W: w, H: h})
}

func (c *connCard) Render(tui.Surface) {}

// HandleEvent owns the copy keys and NOTHING else — every other key falls
// through to the read-only editor, which is what makes the card navigable.
//
// The card NEVER self-dismisses, on success or failure. The secret is shown
// once and cannot be recovered, so the moment of dismissal belongs to the
// person who has to paste it, not to autodb — which cannot know whether the
// paste landed. That property was ratified after manual testing and it would
// be undone by a card that closed on copy.
func (c *connCard) HandleEvent(ev tui.Event) bool {
	k, ok := ev.(tui.KeyEvent)
	if !ok || k.Kind == tui.KeyRelease {
		return false
	}
	if dismissKey(ev) {
		c.float.Hide()
		return true
	}
	for _, cp := range c.copies {
		if k.Text == string(cp.key) {
			c.model.editor.SetRegister(cp.value, false)
			msg, okc, _ := copyReport(c.ctx.CopyToClipboard(cp.value), true)
			if okc {
				c.model.setOK(cp.label + ": " + msg)
			} else {
				c.model.setError(cp.label + ": " + msg)
			}
			return true
		}
	}
	return false
}

func (c *connCard) Add(...tui.Component)    {}
func (c *connCard) Remove(tui.Component)    {}
func (c *connCard) Move(tui.Component, int) {}
func (c *connCard) Children() iter.Seq[tui.Component] {
	return func(yield func(tui.Component) bool) {
		if c.editor != nil {
			yield(c.editor)
		}
	}
}

// cardSSLMode is the one place this surface's client sslmode is written.
//
// It was a literal in two files, which is how the displayed line and the
// copied line come to disagree — and the TLS-off work still to land is exactly
// what will need to change it in one place and not the other.
const cardSSLMode = "verify-full"

// buildCardText renders the card body AND returns the DSN it printed.
//
// The DSN is returned rather than recomputed by the caller because it used to
// be built TWICE — once here for the screen, once in the copy handler for `Y` —
// from two separate computations of host, port and sslmode. A reviewer proved
// the drift with one plausible edit at one site: preferring the last certificate
// name rather than the first made the shown line and the copied line differ
// while all six cells stayed green.
//
// That is the worst shape a bug can take here. The user copies with `Y` and
// pastes without re-reading; the screen is what they trust. One computation,
// one string, handed to both.
func buildCardText(secret string, conn ConnInfo, ep FrontDoorEndpoint, user, expires string) (string, string) {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	if !ep.Configured() {
		// The warning comes FIRST and says which of the two failures it is.
		// Minting a token on an install whose front door is off produces a
		// credential that cannot be used anywhere, and the old reveal said
		// nothing at all.
		p("!! THIS TOKEN CANNOT BE USED YET.")
		switch {
		case !ep.Enabled:
			p("!! The front door is not enabled on this install.")
			p("!! Set [frontdoor] enabled = true in the autodb config, then restart.")
		default:
			p("!! The front door is enabled but NO LISTENER IS RUNNING.")
			p("!! It failed to start — check the daemon log for the reason.")
		}
		p("")
	}

	host, port := splitAddr(ep.Addr)
	dialHost := host
	if len(ep.HostNames) > 0 {
		// verify-full checks the NAME, so the name from the certificate is
		// what a client must dial — not the address it happens to resolve to.
		dialHost = ep.HostNames[0]
	}

	p("connection   %s", conn.Name)
	if conn.TargetDB != "" {
		p("database     %s        <- type THIS into a client's Database field", conn.TargetDB)
	} else {
		p("database     %s        <- this connection has no target database name;", conn.Name)
		p("                          use the connection name")
	}
	p("host         %s", orNone(dialHost))
	p("port         %s", orNone(port))
	p("user         %s", user)
	p("sslmode      %s", cardSSLMode)
	if ep.RootCAFile != "" {
		p("sslrootcert  %s", ep.RootCAFile)
	}
	if expires != "" {
		p("expires      %s", expires)
	}
	if host == "" && ep.Addr != "" {
		p("")
		p("note: the listener is bound to %s, which is not dialable as written.", ep.Addr)
		p("      Use the host name above, which the certificate covers.")
	}
	p("")
	p("token        %s", secret)
	p("")
	dsn := buildCardDSN(dialHost, port, user, secret, cardDatabase(conn), cardSSLMode, ep.RootCAFile)
	p("DSN")
	p("  %s", dsn)
	p("")
	p("JDBC")
	p("  %s", buildCardJDBC(dialHost, port, user, secret, cardDatabase(conn), cardSSLMode, ep.RootCAFile))
	p("")
	p("The token is shown ONCE and cannot be recovered. Copy it before closing.")
	return b.String(), dsn
}

// cardDatabase is what the client should put in its Database field: the
// target's own name when we know it, else the connection name. Both are
// accepted by the front door's consistency check.
func cardDatabase(conn ConnInfo) string {
	if conn.TargetDB != "" {
		return conn.TargetDB
	}
	return conn.Name
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
