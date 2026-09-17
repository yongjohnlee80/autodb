package tui

import (
	"context"
	"fmt"
	"iter"
	"net"
	"strings"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// The connection card: what replaced a float holding nothing but
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
	// keys is the footer, and it is a footer rather than a title because the
	// title was where these lived and a title is read once, before there is
	// anything to copy. It also feeds the `?` overlay, so the two cannot
	// disagree about what the keys are.
	keys   []keyHint
	editor *widget.Editor
	ctx    *tui.Context
	float  *widget.Float
}

// hints puts this card's keys in the overlay and the footer from one list.
func (c *connCard) hints() []keyHint { return c.keys }

// cardCopyKeys and cardKeyHints are the copy bindings and their footer, in ONE
// place each, so a cell can assert what the surfaces actually build rather
// than a copy of it written in a test.
//
// `y` IS ABSENT DELIBERATELY. It used to copy the token, which made a visual
// selection uncopyable: the card claimed the key before the read-only editor
// beneath could treat it as a yank.
func cardCopyKeys(label, all string) []cardCopy {
	return []cardCopy{{'Y', label, all}}
}

func cardKeyHints(allLabel string) []keyHint {
	return []keyHint{
		{"v/V then y", "copy a selection"},
		{"Y", allLabel},
		{"q/Esc", "close"},
	}
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
	// A root CA against a cleartext listener is not merely useless, it is
	// MISLEADING: it is the one parameter in the string that says "this is
	// verified". The two facts are decided here rather than by the caller so
	// they cannot be set half-right.
	if rootCA != "" && sslmode != cardSSLModeOff {
		q += "&sslrootcert=" + rootCA
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?%s", user, secret, host, port, database, q)
}

// buildCardJDBC renders the JetBrains form, because JetBrains is the client
// that surfaced every defect this ADR fixes.
func buildCardJDBC(host, port, user, secret, database, sslmode, rootCA string) string {
	// pgjdbc keys on `ssl` as well as `sslmode`, and ssl=true with
	// sslmode=disable is a contradiction the driver resolves in its own
	// favour. Derived from the one sslmode value rather than written twice.
	ssl := "true"
	if sslmode == cardSSLModeOff {
		ssl = "false"
	}
	q := fmt.Sprintf("user=%s&password=%s&ssl=%s&sslmode=%s", user, secret, ssl, sslmode)
	if rootCA != "" && sslmode != cardSSLModeOff {
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

	// THE EDITOR'S OWN YANK REPORTS ITSELF.
	//
	// A visual selection copied with `y` is handled by the editor, not by this
	// card -- which is the point: `y` means "copy what I selected", the thing
	// a vim user already knows, and the card claiming it would make a
	// selection uncopyable. But CopyToClipboard's result is consumed inside
	// the widget, so without this the operator would see nothing and could not
	// tell a copy that landed from one that never left the process.
	tui.SubscribeScoped(ctx, func(ev widget.YankEvent) {
		if ev.Owner != c.editor.NodeID() {
			return
		}
		msg, okc, _ := copyReport(ev.ClipboardDelivered, true)
		if okc {
			c.model.setOK("selection: " + msg)
		} else {
			c.model.setError("selection: " + msg)
		}
	})
}

func (c *connCard) Layout(cs tui.Constraints) tui.Size {
	w, h := cs.MaxW, cs.MaxH
	// The last row belongs to the footer, so the editor gets one less. A
	// footer drawn OVER the editor would cover a line of the thing being
	// copied, which on a show-once card is unacceptable.
	edH := h
	if len(c.keys) > 0 && edH > 1 {
		edH--
	}
	sz := c.ctx.LayoutChild(c.editor, tui.Tight(tui.Size{W: w, H: edH}))
	c.ctx.PlaceChild(c.editor, tui.Rect{X: 0, Y: 0, W: sz.W, H: sz.H})
	return cs.Constrain(tui.Size{W: w, H: h})
}

func (c *connCard) Render(s tui.Surface) {
	if len(c.keys) == 0 {
		return
	}
	h := s.Size().H
	if h < 1 {
		return
	}
	drawTo(s, 0, h-1, hintLine(c.keys), style.New().Foreground(style.TokenTextMuted))
}

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

// The client sslmode is written in ONE place. It was a literal in two files,
// which is how the displayed line and the copied line come to disagree — and
// the TLS-off work is exactly what a reviewer predicted would change it in one
// of them and not the other.
const (
	cardSSLModeVerify = "verify-full"
	cardSSLModeOff    = "disable"
)

// cardSSLMode is what a client must ask for against THIS listener.
//
// verify-full unless the listener is serving cleartext, in which case a client
// asking for TLS simply cannot connect. Keyed on the live endpoint rather than
// on config, because the card's whole purpose is to describe the listener that
// is actually running.
func cardSSLMode(ep FrontDoorEndpoint) string {
	if ep.Cleartext {
		return cardSSLModeOff
	}
	return cardSSLModeVerify
}

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
func buildCardText(secret string, conn ConnInfo, ep FrontDoorEndpoint, user, role, expires string) (string, string) {
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

	if ep.Cleartext && ep.Configured() {
		// SECOND warning slot, and it is deliberately above the token. A
		// developer who reads only the first two lines of this card must
		// still learn that what they are about to paste travels in the clear.
		p("!! THIS FRONT DOOR IS SERVING WITHOUT TLS.")
		p("!! The token below crosses the network in CLEARTEXT and works from")
		p("!! anywhere it is admitted until it is revoked. Treat it as exposed.")
		p("!! This is a debugging mode — ask the operator to turn TLS back on.")
		p("")
	}

	sslmode := cardSSLMode(ep)
	host, port := splitAddr(ep.Addr)
	dialHost := host
	if len(ep.HostNames) > 0 {
		// verify-full checks the NAME, so the name from the certificate is
		// what a client must dial — not the address it happens to resolve to.
		//
		// Under cleartext no name is checked, but the operator still published
		// these names as how the door is reached, and switching the card to a
		// bare address here would make the two modes disagree about the host
		// for no reason a user could see.
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
	if role != "" {
		// WHAT THIS TOKEN CAN DO, beside who it is. A credential's reach is
		// not inferable from the account name, and the card is the one place
		// the credential is ever shown -- so it is also the one chance to say
		// what was granted.
		p("role         %s", role)
	}
	p("sslmode      %s", sslmode)
	if ep.RootCAFile != "" && sslmode != cardSSLModeOff {
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
	dsn := buildCardDSN(dialHost, port, user, secret, cardDatabase(conn), sslmode, ep.RootCAFile)
	p("DSN")
	p("  %s", dsn)
	p("")
	p("JDBC")
	p("  %s", buildCardJDBC(dialHost, port, user, secret, cardDatabase(conn), sslmode, ep.RootCAFile))
	p("")
	writeCardBudget(p, ep)
	p("The token is shown ONCE and cannot be recovered. Copy it before closing.")
	return b.String(), dsn
}

// writeCardBudget prints the ceilings this token will meet and what to set.
//
// A SHARED CEILING CANNOT YIELD A PRIVATE NUMBER, and three attempts to derive
// one all failed the same way. "Idle should equal open" hoards leases other
// people are waiting for. "Up to your allotted share" named a mechanism that
// does not exist -- admission is first-come-first-served with no per-user
// slices. "The per-user cap divided by the databases you connect to" hands the
// whole shared cap to every process: two Label Manager processes each follow
// four-and-four and together ask for sixteen against a cap of eight, and the
// same breaks with two tokens, two users behind one NAT, or a replica set.
//
// Deriving a share needs inputs this card does not have and cannot have -- how
// many processes this developer will run, how many replicas, who else is
// behind their source address, who is competing right now. So the card stops
// computing. It shows the ceilings WITH THEIR SCOPE, states the multiplier as
// a formula the reader applies to their own situation, gives one worked
// example and says it is one, and then says where the real number comes from:
// an operator.
//
// NOT LIVE AVAILABILITY, EITHER. This card is shown once and cannot be
// recovered, so a count of backends free at this instant is stale before it is
// read. Live figures belong in the pressure view, which carries a timestamp.
//
// THIS BLOCK IS A BRIDGE. The acceptance rules say a developer configures
// nothing; today they must, or the front door refuses them. When the scheduler
// lands this is demoted to latency advice or removed, and that obligation is a
// capability test carried by the change that makes it obsolete -- not a note
// here, because a plan to delete something later is the kind of plan that does
// not happen.
func writeCardBudget(p func(string, ...any), ep FrontDoorEndpoint) {
	// Nothing to say when the door is not configured: the warning at the top
	// of the card already says the token cannot be used anywhere.
	if !ep.Configured() {
		return
	}

	p("LIMITS THAT APPLY TO THIS TOKEN")
	p("  These are ceilings, and they are SHARED. None of them is yours alone.")
	p("")
	p("  %-22s %-9s %s", "sessions per user", cardCap(ep.MaxSessionsPerUser),
		"you, across every database you connect to")
	p("  %-22s %-9s %s", "sessions, instance", cardCap(ep.MaxSessionsGlobal),
		"everyone using this autodb")
	p("  %-22s %-9s %s", "backend connections", cardCap(ep.MaxTargetConns),
		"everyone, across every target database")
	p("")
	// The per-source row is ABSENT, not approximated. What exists per source
	// today is a credential and TLS failure count in a window, and the
	// temporary throttle that follows it -- a rate limit on FAILURES, not a
	// ceiling on concurrent capacity. Printing the throttle here as though it
	// were a concurrency cap would teach exactly the wrong model of what is
	// limiting somebody, which is the confusion the incident behind this work
	// was made of.
	p("  Your demand is  processes x databases x connections-per-pool.")
	p("  A second process doubles it. Apply the formula to your own setup; the")
	p("  card cannot know how many processes or replicas you will run, or who")
	p("  else is behind your address.")
	p("")
	p("  Worked example, and it is an EXAMPLE, not a rule:")
	p("    one process, two databases, nobody else competing, cap of eight")
	p("    -> four connections per pool.")
	p("")
	p("  Your real number is an allocation the OPERATOR makes. Ask them.")
	p("")
	p("WHAT TO SET, FOR THE CLIENT YOU ARE USING")
	p("  Label Manager (lm-http, gold-http)")
	p("    PG_MAX_OPEN_CONNS / PG_MAX_IDLE_CONNS")
	p("    -- Label Manager's OWN environment variables, read by its config.")
	p("       Not a Postgres setting and not a database/sql one.")
	p("  Any other Go database/sql application")
	p("    pool.SetMaxOpenConns(n) / pool.SetMaxIdleConns(n)")
	p("  A JDBC application using HikariCP")
	p("    maximumPoolSize / minimumIdle")
	p("  DataGrip")
	p("    its own connection-pool setting, in the data-source options.")
	p("    NOT a HikariCP property, and not something an application sets.")
	p("  psql")
	p("    nothing -- one connection per session.")
	p("")
	p("  Reopening is not free: every new connection costs a TLS handshake, a")
	p("  token verification and a fresh admission.")
	p("")
}

// cardCap renders one ceiling, or says the daemon did not report it.
//
// ZERO IS NOT A CAP OF ZERO. An older daemon answering a newer frontend sends
// nothing for these fields, and printing "0" there tells a developer they may
// open no sessions at all -- a confident wrong answer where the honest one is
// that this daemon does not say.
func cardCap(n int) string {
	if n <= 0 {
		return "not reported"
	}
	return fmt.Sprintf("%d", n)
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

// openCAcert shows the front door's CA CERTIFICATE ITSELF, in a read-only vim
// editor float that can be selected from and copied out of.
//
// The contents, not the path, and that is the whole point. `sslmode=verify-full`
// plus this one file is what every client needs, and the path was useless to
// the person who needs it: a developer running the TUI over a tunnel cannot
// read a file on the daemon's host, and on the host itself /etc/autodb/tls is
// 0710 -- traversal for the service account and root, nobody else. The card
// used to print the path for exactly this purpose and it was removed as noise;
// this is the surface that actually serves it.
//
// Same component as the token card, so the keys are the same ones: a visual
// selection with `y`, everything with `Y`, and a footer that says so.
func (m *Model) openCAcert() {
	bound := m.session.Bind()
	m.ctx.Go(func(c context.Context) (any, error) {
		ca, err := bound.FrontDoorCAPem(c)
		if err != nil {
			msg := WireErrorMessage(err)
			return managerReload{gen: bound.Gen(), apply: func() {
				m.setError("CA certificate: " + msg)
			}}, nil
		}
		return managerReload{gen: bound.Gen(), apply: func() { m.showCAcert(ca) }}, nil
	})
}

func (m *Model) showCAcert(ca CAPem) {
	if ca.SystemRoots {
		// NOT an empty document. An install with no private CA is a different
		// answer from one whose certificate could not be read, and a blank
		// float would leave a reader unable to tell which they got.
		m.openTextFloat("front-door CA certificate",
			"This install has no private CA: [frontdoor] tls_root_ca_file is unset,\n"+
				"so clients verify against their own system roots and there is no file\n"+
				"to distribute.\n\n"+
				"If the front door is using a private CA, set tls_root_ca_file in the\n"+
				"daemon's config -- without it a verify-full client fails with\n"+
				"\"unknown authority\" and the error names the certificate's HOST NAMES,\n"+
				"which reads like a name mismatch and sends you the wrong way.\n")
		return
	}
	if strings.TrimSpace(ca.PEM) == "" {
		m.setError("CA certificate: the daemon returned an empty document for its configured " +
			"CA file ([frontdoor] tls_root_ca_file)")
		return
	}
	// THE CERTIFICATE ALONE. No path, anywhere on this surface.
	//
	// A footnote naming the file on the daemon's host was here, and a review
	// caught it reintroducing the very thing the operator had asked to be
	// removed from the reveal card. Two reasons beyond that ask: `SPC k` is
	// offered to EVERY developer by design -- the certificate is the file you
	// hand out -- so the footnote disclosed the daemon's filesystem layout to
	// people who cannot read it and did not need it; and a PEM document with a
	// trailing line of prose is one somebody selects whole in their terminal
	// and pastes into a client.
	body := ca.PEM
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	card := &connCard{
		model:  m,
		text:   body,
		copies: cardCopyKeys("the CA certificate", ca.PEM),
		keys:   cardKeyHints("copy the certificate"),
	}
	card.float = m.openFloatPct("front-door CA certificate — hand this to clients",
		card, scriptPct)
}
