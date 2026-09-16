package tui

import (
	"context"
	"slices"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// Select rows: the fields whose vocabulary the program already knows.
//
// A label that enumerates its valid values — "engine (postgres | mysql |
// sqlite)" — is an input admitting it cannot show them, and "connection id"
// is worse: it asks the operator to leave the form, find a list, remember a
// number and come back. These rows carry the vocabulary instead.

// loadState is where a live select is between being opened and having options.
//
// The three are kept APART because conflating any two of them lies to the
// operator: an empty list and a failed fetch look identical on screen and mean
// opposite things — one says "create one first", the other says "the server did
// not answer".
type loadState uint8

const (
	loadPending loadState = iota
	loadReady
	loadFailed
)

// selectControl is one select row. T is the type of the value it yields, which
// is the id for the id fields and the word itself for the enums.
type selectControl[T comparable] struct {
	sel   *widget.Select[T]
	state loadState
	count int
	err   error
}

func (c *selectControl[T]) component() tui.Component { return c.sel }

// value yields the typed selection, or nil when the operator has chosen
// nothing. nil rather than the zero value: for an id field, 0 is not "no
// choice", it is a row number that does not exist, and the two must not
// arrive at the backend looking the same.
func (c *selectControl[T]) value() any {
	v, ok := c.sel.Value()
	if !ok {
		return nil
	}
	return v
}

// annotation is what the row's label says about the load, and it is the whole
// of D3's three states.
func (c *selectControl[T]) annotation() string {
	switch c.state {
	case loadPending:
		return " — loading…"
	case loadFailed:
		return " — could not load: " + WireErrorMessage(c.err)
	}
	if c.count == 0 {
		return " — none yet"
	}
	return ""
}

// blocks refuses a submit the operator cannot have meant. A form submitted
// while its options are still arriving would send whatever was chosen from an
// incomplete list, and one submitted after a failed load would send nothing
// from a list that was never shown.
func (c *selectControl[T]) blocks() (bool, string) {
	switch c.state {
	case loadPending:
		return true, "still loading the options"
	case loadFailed:
		return true, "the options could not be loaded: " + WireErrorMessage(c.err)
	}
	return false, ""
}

// labelAnnotator is a control whose row label has something to add.
type labelAnnotator interface{ annotation() string }

// submitGate is a control that can refuse a submit, with a reason.
type submitGate interface{ blocks() (bool, string) }

// staticSelect is a field whose vocabulary is fixed and known here: the engines
// and the roles. It is NOT filtered — a three-item list needs no search box.
func staticSelect(label string, items []widget.SelectItem[string]) formField {
	return formField{label: label, build: func(f *form, i int) formControl {
		return &selectControl[string]{
			sel:   widget.NewSelect(widget.WithOptions(items)),
			state: loadReady,
			count: len(items),
		}
	}}
}

// liveSelect is a field whose options come from the backend.
//
// THE LOAD CLOSES OVER THE BOUND IT WAS ISSUED ON, and the result is applied
// only if four things still hold. A form is a long-lived surface — it outlives
// list refreshes, reconnects and, on a shared terminal, sign-ins — so each of
// these is a way for options to arrive somewhere they do not belong:
//
//   - THE SESSION EPOCH still matches. Enforced one level up, by the
//     managerReload dispatcher, which drops any result whose pinned gen is not
//     the live one — so it is not repeated here. Duplicating it would leave two
//     guards for one rule and no answer to which is authoritative.
//   - THE FORM IS STILL OPEN, or the options land on a surface the operator
//     dismissed, or worse, on the next form to occupy the same variable.
//   - THE IDENTITY still matches. A same-connection sign-in switch does NOT
//     bump the epoch, so the check above cannot see it — the same hole that
//     once rendered one person's token in a DSN naming another, which is why
//     Bound pins the user alongside the credential.
//
// A result failing either of the checks made here is DISCARDED, never merged
// "just in case": a half-current list is the one outcome worse than an empty
// one.
//
// There is deliberately NO latest-load check. A control issues exactly one
// load, so there is no second result to arrive out of order, and a sequence
// guard here would be a guard nothing could exercise. The day a field reloads
// — options that depend on another field's choice would do it — is the day it
// earns one.
func liveSelect[T comparable](m *Model, label string,
	load func(context.Context, *Bound) ([]widget.SelectItem[T], error)) formField {

	return formField{label: label, build: func(f *form, i int) formControl {
		c := &selectControl[T]{
			// Filtered: a connection list is as long as the deployment, and a
			// plain list of fifty is its own usability problem.
			sel:   widget.NewSelect(widget.WithFilter[T](true)),
			state: loadPending,
		}
		bound := m.session.Bind()
		m.ctx.Go(func(ctx context.Context) (any, error) {
			items, err := load(ctx, bound)
			return managerReload{gen: bound.Gen(), apply: func() {
				if !f.open() || !m.sameIdentity(bound) {
					return
				}
				if err != nil {
					c.state, c.err = loadFailed, err
				} else {
					c.sel.SetOptions(items)
					c.state, c.count, c.err = loadReady, len(items), nil
				}
				f.refreshLabels()
			}}, nil
		})
		return c
	}}
}

// sameIdentity reports whether work issued on b belongs to the session signed
// in now.
//
// IT COMPARES EPOCHS, NOT USER IDS. An id says WHO, and the question here is
// WHICH SESSION: the same person signing out and back in holds a new token and
// a new session, with the same id, and options fetched for the old one must not
// be rendered into a form the new one is looking at. An id comparison cannot
// see that case at all.
//
// The connection epoch is deliberately not checked here — the managerReload
// dispatcher already refuses a superseded one, and a second copy of that rule
// would leave no answer to which is authoritative.
func (m *Model) sameIdentity(b *Bound) bool {
	if b == nil || m.session == nil {
		return false
	}
	return b.IdentityEpoch() == m.session.IdentityEpoch()
}

// engineItems and roleItems are the two closed vocabularies this product has.
// They were in field LABELS until now, which is the whole defect.

// engineItems is built from engine.All() rather than from three literals.
//
// core/engine owns these names and guards them: a literal here is an engine
// identity written as text, where a typo is a silent no-match rather than a
// compile error. Deriving the list also means a FOURTH engine appears in this
// select by existing, instead of by somebody remembering to add it — which is
// the same class of omission the select exists to remove from the operator.
func engineItems() []widget.SelectItem[string] {
	names := engine.All()
	items := make([]widget.SelectItem[string], 0, len(names))
	for _, n := range names {
		items = append(items, widget.SelectItem[string]{Label: string(n), Value: string(n)})
	}
	return items
}

func roleItems() []widget.SelectItem[string] {
	return []widget.SelectItem[string]{
		{Label: "admin — everything, including users", Value: meta.RoleAdmin},
		{Label: "editor — read and write data", Value: meta.RoleEditor},
		{Label: "reader — read only", Value: meta.RoleReader},
	}
}

// --- the catalogues -----------------------------------------------------------
//
// THE CATALOGUE IS ADDRESSED TO THE OPERATION, NOT TO THE NOUN. "Every
// connection" is the right answer to almost none of these questions: detaching
// offers what is attached, attaching offers what is not, and an operator handed
// the wrong set will pick from it and learn they were wrong from a refusal that
// arrives after the form has gone.

// connectionOptions offers the connections the signed-in user can see, which is
// the grant catalogue: an admin may grant another user access to any connection
// the admin can reach, and the server refuses the rest.
func connectionOptions(ctx context.Context, b *Bound) ([]widget.SelectItem[int64], error) {
	return connectionItems(ctx, b, nil)
}

// reachableConnectionOptions is the TOKEN catalogue, and it is narrower: a
// personal access token is used from outside, so a connection the front door
// does not expose is one the token could never reach. Offering it would invite
// an operator to mint a credential that cannot work and learn why later.
func reachableConnectionOptions(ctx context.Context, b *Bound) ([]widget.SelectItem[int64], error) {
	return connectionItems(ctx, b, func(c ConnInfo) bool { return c.FrontDoorExposed })
}

// connectionItems lists the caller's connections, optionally narrowed.
func connectionItems(ctx context.Context, b *Bound,
	keep func(ConnInfo) bool) ([]widget.SelectItem[int64], error) {
	rows, err := b.Connections(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]widget.SelectItem[int64], 0, len(rows))
	for _, r := range rows {
		if keep != nil && !keep(r) {
			continue
		}
		items = append(items, widget.SelectItem[int64]{
			Label: r.Name + " (" + r.Engine + ")", Value: r.ID,
		})
	}
	return items, nil
}

// workspacesWithout offers the workspaces this connection is NOT already
// attached to, which is the attach question. Offering the ones it is already in
// invites an operator to ask for something the server will refuse.
func workspacesWithout(connID int64) func(context.Context, *Bound) ([]widget.SelectItem[int64], error) {
	return func(ctx context.Context, b *Bound) ([]widget.SelectItem[int64], error) {
		rows, err := b.Workspaces(ctx)
		if err != nil {
			return nil, err
		}
		items := make([]widget.SelectItem[int64], 0, len(rows))
		for _, w := range rows {
			if slices.ContainsFunc(w.Connections, func(c ConnInfo) bool { return c.ID == connID }) {
				continue
			}
			items = append(items, widget.SelectItem[int64]{Label: w.Name, Value: w.ID})
		}
		return items, nil
	}
}

// attachedItems is the detach catalogue, and it needs no load at all: the
// workspace row the operator is standing on already carries its connections.
// Fetching the whole server's list to filter it down would be slower and could
// disagree with the row on screen.
func attachedItems(ws WorkspaceInfo) []widget.SelectItem[int64] {
	items := make([]widget.SelectItem[int64], 0, len(ws.Connections))
	for _, c := range ws.Connections {
		items = append(items, widget.SelectItem[int64]{
			Label: c.Name + " (" + c.Engine + ")", Value: c.ID,
		})
	}
	return items
}

// fixedSelect is a select over a catalogue already in hand — no load, so no
// loading state and nothing to fail. Detach uses it.
func fixedSelect(label string, items []widget.SelectItem[int64]) formField {
	return formField{label: label, build: func(f *form, i int) formControl {
		return &selectControl[int64]{
			sel:   widget.NewSelect(widget.WithFilter[int64](true), widget.WithOptions(items)),
			state: loadReady,
			count: len(items),
		}
	}}
}

// --- the editor profile -------------------------------------------------------

// keysetOf maps the stored preference onto golib's profile. golib spells the
// modeless standard profile KeysetStandard; the product calls it TextEdit, and
// the two names meet here rather than in five call sites.
func keysetOf(pref string) widget.Keyset {
	if pref == auth.KeysetTextEdit {
		return widget.KeysetStandard
	}
	return widget.KeysetVim
}

// resolveEditorPref decides which profile an account's stored options mean.
//
// AN ABSENT KEY MEANS VIM, and that is the whole of a real defect: the read path
// used to skip the SetKeyset entirely when an account had never chosen, which
// left the editor on whatever the PREVIOUS account had selected. It lives in its
// own function so the cell that checks it calls THIS, rather than a copy of it
// that agrees with itself.
func resolveEditorPref(opts map[string]string) string {
	if pref, ok := opts[auth.OptionEditorKeyset]; ok {
		return pref
	}
	return auth.KeysetVim
}

// prefIntent is one preference choice, captured WHOLE at the moment it was made.
//
// Every field here answers a question that cannot be asked later. Re-reading the
// session at dispatch time is what lets one account's queued choice go out under
// another account's credential: the queue outlives the sign-in that filled it.
type prefIntent struct {
	pref string
	// gen is m.prefGen at the choice, so a completion knows whether the
	// operator has since chosen something else.
	gen uint64
	// epoch is m.identityEpoch at the choice: WHO chose it.
	epoch uint64
	// bound is the credential it was chosen under, pinned rather than looked up
	// when the write finally goes out.
	bound *Bound
}

// prefWritten is a writer ticket coming back. It is handled unconditionally —
// see the dispatcher — because a ticket that can be dropped is a writer that
// can stay busy forever.
type prefWritten struct {
	// ticket is the writer slot this completion was issued for. A completion
	// whose ticket is no longer active owns nothing: it must not clear the
	// writer, drain the queue, or report.
	ticket uint64
	intent prefIntent
	err    error
}

// optionWriter is the preference RPC. It takes the Bound EXPLICITLY, which is
// the point: a writer that reached for the session instead would send one
// person's choice under whoever is signed in when it runs.
func optionWriter(ctx context.Context, b *Bound, pref string) error {
	return b.SetOption(ctx, auth.OptionEditorKeyset, pref)
}

// optionReader is the other half, and it is indirected for the same reason the
// writer is: the ORDERING between a stored read and a later menu choice is the
// thing under test, and a cell cannot schedule it without being able to hold
// the read open.
func optionReader(ctx context.Context, b *Bound) (map[string]string, error) {
	return b.Options(ctx)
}

// chooseEditorKeyset switches the editor now and remembers the choice.
//
// THE LIVE EDITOR CHANGES IN PLACE, which is why this phase waited on golib
// v0.5.24: until SetKeyset existed the only way to honour the choice was to
// build a new Editor, and that throws away the buffer, the cursor and the undo
// history. A preference change is not a reason to lose someone's work.
//
// The switch happens FIRST and the write follows. The operator asked for a
// different editor, and they get one whether or not the daemon accepts the
// preference; a failed write costs them the persistence, not the change.
func (m *Model) chooseEditorKeyset(pref string) {
	m.prefGen++
	m.editor.SetKeyset(keysetOf(pref))
	m.refreshStatus()
	m.submitPref(prefIntent{
		pref:  pref,
		gen:   m.prefGen,
		epoch: m.identityEpoch,
		bound: m.session.Bind(),
	})
}

// submitPref writes one choice, ONE AT A TIME.
//
// Two writes in flight can be persisted in either order and the store has no
// opinion about which the operator made last, so the second choice can lose to
// the first. Only one is outstanding; a choice made while it is out REPLACES
// any other waiting one, because the operator's latest answer is the only one
// worth writing.
func (m *Model) submitPref(in prefIntent) {
	if m.prefActive != 0 {
		queued := in
		m.prefPending = &queued
		return
	}
	m.prefTicket++
	ticket := m.prefTicket
	m.prefActive = ticket
	write := m.writeOption
	if write == nil {
		write = optionWriter
	}
	m.ctx.Go(func(ctx context.Context) (any, error) {
		// in.bound, NEVER m.session.Bind(). The credential is the one the
		// operator chose under; re-binding here is the defect the capture
		// exists to prevent.
		return prefWritten{ticket: ticket, intent: in, err: write(ctx, in.bound, in.pref)}, nil
	})
}

// settlePrefWrite returns the writer ticket and decides what to do next.
//
// THE TICKET COMES BACK FIRST, before any currency test. Everything after this
// line is about whether the result is worth reporting or the queue is worth
// draining; none of it may decide whether another preference can ever be
// written.
func (m *Model) settlePrefWrite(v prefWritten) {
	// A COMPLETION THAT NO LONGER OWNS THE WRITER OWNS NOTHING. It arrives
	// after its identity was retired, or after its slot was taken; clearing the
	// writer here would free a slot somebody else is using, draining the queue
	// would dispatch their choice on this one's completion, and reporting would
	// put a departed session's message on screen.
	if v.ticket != m.prefActive {
		return
	}
	m.prefActive = 0

	// Reported only to the person who chose it, and only if they have not
	// chosen again since. A failure to save a choice already replaced is noise
	// about a decision the operator has moved on from.
	if m.current(v.intent.epoch) && v.intent.gen == m.prefGen {
		if v.err != nil {
			m.setStatus("editor set to " + v.intent.pref +
				" for this session only — saving it failed: " + WireErrorMessage(v.err))
		} else {
			m.setStatus("editor mode: " + v.intent.pref)
		}
	}

	next := m.prefPending
	m.prefPending = nil
	if next == nil {
		return
	}
	// A QUEUED CHOICE IS DISPATCHED ONLY FOR THE IDENTITY THAT MADE IT. Signing
	// out while a write is in flight leaves the queued one belonging to nobody
	// present; sending it would write A's preference using B's token.
	if !m.current(next.epoch) {
		return
	}
	m.submitPref(*next)
}

// applyStoredEditorKeyset reads the account's preference after a sign-in and
// applies it.
//
// A MISSING PREFERENCE IS A PREFERENCE FOR THE DEFAULT, and saying so is the fix
// for a real leak: this used to return early when an account had never chosen,
// which left the editor on whatever the PREVIOUS account had selected. Sign in
// as someone who likes TextEdit, sign out, sign in as someone who has never
// chosen, and they get TextEdit — one person's setting applied to another's
// session, silently.
//
// It also loses to a later choice. The read is issued at sign-in and may land
// after the operator has picked from the menu; the menu is the newer intent.
func (m *Model) applyStoredEditorKeyset() {
	bound := m.session.Bind()
	gen := m.prefGen
	epoch := m.identityEpoch
	read := m.readOptions
	if read == nil {
		read = optionReader
	}
	m.ctx.Go(func(ctx context.Context) (any, error) {
		opts, err := read(ctx, bound)
		return managerReload{gen: bound.Gen(), apply: func() {
			if !m.current(epoch) {
				return // signed in as somebody else since; their preference, not this one
			}
			if gen != m.prefGen {
				return // the operator has chosen since this was asked for
			}
			if err != nil {
				m.setStatus("could not read your preferences: " + WireErrorMessage(err))
				return
			}
			m.editor.SetKeyset(keysetOf(resolveEditorPref(opts)))
			m.refreshStatus()
		}}, nil
	})
}
