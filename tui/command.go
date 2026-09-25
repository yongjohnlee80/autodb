package tui

// THE COMMAND CATALOG — one declaration per thing the TUI can do, and the
// surfaces that offer it.
//
// Every command used to be an anonymous entry in one list, which was fine while
// there was one menu. A second one (the top menu bar) makes two
// hand-maintained lists, and they disagree the first time somebody adds a
// feature to one of them — nothing fails when they do, the feature is simply unreachable from
// half the product and the help screen tells a third story.
//
// So behaviour is declared once and each surface PROJECTS from it. The
// guarantee is deliberately narrower than "adding a command adds it
// everywhere", which is false: several commands are legitimately leader-only.
// It is that one catalog declares every membership in one place, and
// validation rejects the accidental omissions.
//
// WHAT THIS IS NOT: an authorization layer. Audience decides what is
// ADVERTISED. The server re-resolves the caller's role on every RPC, and the
// local lifecycle commands — quit, restart, connect — have no server to appeal
// to and enforce their own capability inside their handler. A reader who
// reaches this file looking for the security boundary is in the wrong file.

import (
	"fmt"
	"sort"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// CommandID is behaviour identity. It is stable, it is what a menu row carries
// as its action, and it is never shown to a user.
type CommandID string

// MenuNodeID names a category or submenu in the bar's tree. Stable, like a
// CommandID, and for the same reason: a display label is mutable and
// localizable, and hanging the hierarchy off one means the tree changes shape
// when somebody rewords a word.
type MenuNodeID string

// Lifecycle separates "does not exist yet" from "cannot be used right now".
//
// They are NOT the same and encoding both as unavailability makes the audit
// test unable to prove its own claim: a planned command and a temporarily
// withdrawn one become indistinguishable, so "no planned command is reachable"
// degenerates into "nothing currently unavailable is reachable".
type Lifecycle uint8

const (
	// Implemented commands have a handler and may be offered.
	Implemented Lifecycle = iota
	// Planned commands are declared so the gap between the designed menu and
	// the built product is auditable in one place. They carry no handler and
	// are never projected into a live surface.
	Planned
)

// Audience is who a command is ADVERTISED to. Presentation only — see the file
// comment.
type Audience uint8

const (
	// AudienceAll is offered to every signed-in role.
	AudienceAll Audience = iota
	// AudienceEditorAndAdmin excludes readers.
	AudienceEditorAndAdmin
	// AudienceAdmin is admin-only.
	AudienceAdmin
)

// VisibleTo reports whether a role should be SHOWN this command.
//
// AudienceAll means ALL, unknown and empty roles included. That is not
// sloppiness about the failure direction — it is the pre-login state. The role
// is "" until a session is established, and the leader menu is reachable then;
// ranking "" below reader would blank the entire menu before anybody had a
// chance to log in, which a parity cell caught.
//
// For the PRIVILEGED levels the conservative reading applies and an unknown
// role ranks lowest, so a missing or misspelled role string can never advertise
// an admin command.
func (a Audience) VisibleTo(role string) bool {
	if a == AudienceAll {
		return true
	}
	rank := 0
	switch role {
	case meta.RoleReader:
		rank = 1
	case meta.RoleEditor:
		rank = 2
	case meta.RoleAdmin:
		rank = 3
	}
	if a == AudienceEditorAndAdmin {
		return rank >= 2
	}
	return rank >= 3
}

// foldRune is the case fold the MENU WIDGET uses to match a mnemonic.
//
// It must agree with upstream exactly. The widget folds ASCII, so a category
// keyed 'S' and a leaf keyed 's' are ONE mnemonic at runtime — and validation
// that compares raw runes accepts them as two, leaving whichever the widget
// finds second unreachable by its own key. Keyed by the folded rune, the
// collision is caught where it is declared.
func foldRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// LeaderProjection is how a command appears on the SPC which-key menu.
//
// The leader's labels are deliberately long and descriptive ("run query
// (selection when active)") where a menu leaf wants "Execute". One shared title
// would force one of the two surfaces to read wrong, so each owns its own.
type LeaderProjectionOf[H CommandHost] struct {
	Key   rune
	Label string
	// LabelFor overrides Label when non-nil, for the one entry whose text is
	// state-dependent: `x` reads "disconnect" while connected and "connect"
	// while not. Preserving that exactly is what makes the parity test an
	// oracle rather than an approximation.
	LabelFor func(H) string
	Order    int
	// Help is the longer explanation, shown indented under the row.
	//
	// It exists because the help screen used to REPEAT four bindings in a
	// second block with better wording than the menu's own — so SPC C appeared
	// twice, saying two different things, and a reader had to guess which was
	// current. The wording moved here; the binding now appears once.
	Help string
}

// text resolves the label for a state.
func (p LeaderProjectionOf[H]) text(h H) string {
	if p.LabelFor != nil {
		return p.LabelFor(h)
	}
	return p.Label
}

// MenuProjection is how a command appears on the top menu bar.
//
// Parent is the leaf's IMMEDIATE parent and nothing more. Ancestry is derived
// from the node catalog, so there is exactly one parent graph; a full path
// copied onto every command would be a second one, free to disagree with the
// first.
type MenuProjection struct {
	Parent MenuNodeID
	Label  string
	Hotkey rune
	Order  int
}

// MenuNode is a category or submenu. Behaviour-free: the tree holds structure
// and text, the catalog holds what happens.
type MenuNode struct {
	ID     MenuNodeID
	Parent MenuNodeID // empty only for a top-level category
	Label  string
	Hotkey rune
	Order  int
}

// OfferState is how a command appears right now.
type OfferState uint8

const (
	// OfferOffered is usable: a normal row.
	OfferOffered OfferState = iota
	// OfferDisabled could be usable but is not right now: visible, dimmed, and
	// carrying a reason so the operator does not have to guess why.
	OfferDisabled
	// OfferHidden can never be usable for this person here: absent.
	OfferHidden
)

// Offering is the resolved presentation of a command, computed once and
// consumed by every surface. No surface recomputes it, which is what stops the
// bar and the leader menu developing different ideas about the same command.
type Offering struct {
	State  OfferState
	Reason string // non-empty iff State == OfferDisabled
}

// CommandHost is what a command runs against: the program that owns the
// session. Its role decides the command's audience.
type CommandHost interface {
	commandRole() string
}

// Command, LeaderProjection and Catalog are the catalog over the terminal
// Model; a program with another host instantiates the Of forms.
type (
	Command          = CommandOf[*Model]
	LeaderProjection = LeaderProjectionOf[*Model]
	Catalog          = CatalogOf[*Model]
)

func (m *Model) commandRole() string { return m.session.User().Role }

// CommandOf is one thing the TUI can do.
type CommandOf[H CommandHost] struct {
	ID        CommandID
	Lifecycle Lifecycle
	Audience  Audience
	// Visible is frontend and capability withdrawal: false HIDES the command.
	// Never the role — putting that here too would create two fields that can
	// disagree about one question, and Audience is the one that answers it.
	// nil means visible.
	Visible func(H) bool
	// Enabled is TRANSIENT refusal: false leaves the command visible but
	// dimmed, carrying the returned reason. nil means enabled.
	//
	// The line between this and Visible is whether the thing the command acts
	// on EXISTS. The panes exist and none is zoomed, so "zoom out" is dimmed
	// and says why; there is no session to switch on a frontend that does not
	// own one, so that command is absent rather than permanently greyed.
	Enabled func(H) (bool, string)
	Run     func(H)

	Leader *LeaderProjectionOf[H] // nil: deliberately not on the leader menu
	Menu   []MenuProjection       // empty: deliberately not on the bar
}

// offering resolves the command's presentation, strictest first.
//
// The order is fixed and the answers differ, so it is written once here rather
// than reconstructed per surface. A false Enabled MUST supply a reason: a blank
// tooltip is a worse answer than no tooltip, and catching it at the boundary
// turns an empty string into a visible bug rather than a silent one.
func (c *CommandOf[H]) offering(h H) Offering {
	if c.Lifecycle != Implemented {
		return Offering{State: OfferHidden}
	}
	if !c.Audience.VisibleTo(h.commandRole()) {
		return Offering{State: OfferHidden}
	}
	if c.Visible != nil && !c.Visible(h) {
		return Offering{State: OfferHidden}
	}
	if c.Enabled != nil {
		if ok, reason := c.Enabled(h); !ok {
			if reason == "" {
				// A PROGRAMMING ERROR, RAISED AS ONE. Two softer versions of
				// this shipped first and both were wrong: a friendly substitute
				// string hid the broken predicate behind a plausible row, and a
				// Faulty flag that no production surface read was a marker for
				// nobody — the malformed predicate still reached the operator
				// either way, so there was no boundary at all.
				//
				// Same policy as a malformed catalog, which panics in New: a
				// declaration this package got wrong is not a runtime condition
				// to degrade around.
				panic("tui: command " + string(c.ID) +
					": Enabled refused without a reason")
			}
			return Offering{State: OfferDisabled, Reason: reason}
		}
	}
	return Offering{State: OfferOffered}
}

// offered is the activation gate: only an OfferOffered command may run.
func (c *CommandOf[H]) offered(h H) bool {
	return c.offering(h).State == OfferOffered
}

// Catalog is the validated, immutable set of commands and menu nodes.
//
// Built once, during New, after the Model's components exist. Projections
// re-evaluate state on every open; identity and closures are never rebuilt,
// because a command that is a different value each time it is read cannot be
// compared, cached or trusted to be the same command the user saw.
type CatalogOf[H CommandHost] struct {
	commands []CommandOf[H]
	nodes    []MenuNode
	byID     map[CommandID]*CommandOf[H]
	nodeByID map[MenuNodeID]*MenuNode
}

// NewCatalog validates and freezes a catalog, or reports why it cannot.
//
// Validation is a constructor rather than a test helper on purpose: a malformed
// catalog is a programming error that must not reach a user as a missing menu
// row, and the one place that can catch every case is the place that builds it.
func NewCatalog(cmds []Command, nodes []MenuNode) (*Catalog, error) {
	return NewCatalogOf(cmds, nodes)
}

// NewCatalogOf is NewCatalog for a catalog over any host.
func NewCatalogOf[H CommandHost](cmds []CommandOf[H], nodes []MenuNode) (*CatalogOf[H], error) {
	c := &CatalogOf[H]{
		commands: append([]CommandOf[H](nil), cmds...),
		nodes:    append([]MenuNode(nil), nodes...),
		byID:     make(map[CommandID]*CommandOf[H], len(cmds)),
		nodeByID: make(map[MenuNodeID]*MenuNode, len(nodes)),
	}
	for i := range c.nodes {
		n := &c.nodes[i]
		if n.ID == "" {
			return nil, fmt.Errorf("menu node %d has no id", i)
		}
		if _, dup := c.nodeByID[n.ID]; dup {
			return nil, fmt.Errorf("menu node %q declared twice", n.ID)
		}
		c.nodeByID[n.ID] = n
	}
	// Known parents, and no cycles. A cycle would make the projection walk
	// forever rather than draw a wrong tree, so it is worth its own check.
	for _, n := range c.nodes {
		if n.Parent == "" {
			continue
		}
		if _, ok := c.nodeByID[n.Parent]; !ok {
			return nil, fmt.Errorf("menu node %q names unknown parent %q", n.ID, n.Parent)
		}
		seen := map[MenuNodeID]bool{n.ID: true}
		for p := n.Parent; p != ""; {
			if seen[p] {
				return nil, fmt.Errorf("menu node %q is its own ancestor", n.ID)
			}
			seen[p] = true
			p = c.nodeByID[p].Parent
		}
	}
	// Sibling collisions, in order and in hotkey. Two rows claiming one key is
	// a menu where the second is unreachable.
	type sib struct {
		order  map[int]MenuNodeID
		hotkey map[rune]MenuNodeID
	}
	sibs := map[MenuNodeID]*sib{}
	for _, n := range c.nodes {
		s := sibs[n.Parent]
		if s == nil {
			s = &sib{order: map[int]MenuNodeID{}, hotkey: map[rune]MenuNodeID{}}
			sibs[n.Parent] = s
		}
		if prev, dup := s.order[n.Order]; dup {
			return nil, fmt.Errorf("menu nodes %q and %q share order %d under %q",
				prev, n.ID, n.Order, n.Parent)
		}
		s.order[n.Order] = n.ID
		if n.Hotkey != 0 {
			if prev, dup := s.hotkey[foldRune(n.Hotkey)]; dup {
				return nil, fmt.Errorf("menu nodes %q and %q share hotkey %q under %q",
					prev, n.ID, n.Hotkey, n.Parent)
			}
			s.hotkey[foldRune(n.Hotkey)] = n.ID
		}
	}

	leaderKeys := map[rune]CommandID{}
	for i := range c.commands {
		cmd := &c.commands[i]
		if cmd.ID == "" {
			return nil, fmt.Errorf("command %d has no id", i)
		}
		if _, dup := c.byID[cmd.ID]; dup {
			return nil, fmt.Errorf("command %q declared twice", cmd.ID)
		}
		c.byID[cmd.ID] = cmd

		switch cmd.Lifecycle {
		case Implemented:
			if cmd.Run == nil {
				return nil, fmt.Errorf("command %q is Implemented with no handler", cmd.ID)
			}
		case Planned:
			if cmd.Run != nil {
				return nil, fmt.Errorf("command %q is Planned but carries a handler", cmd.ID)
			}
		}
		if cmd.Leader == nil && len(cmd.Menu) == 0 {
			return nil, fmt.Errorf("command %q is on no surface; declare a projection "+
				"or remove it", cmd.ID)
		}
		if cmd.Leader != nil {
			k := cmd.Leader.Key
			if k == 0 {
				return nil, fmt.Errorf("command %q has a leader projection with no key", cmd.ID)
			}
			if prev, dup := leaderKeys[k]; dup {
				return nil, fmt.Errorf("commands %q and %q both claim leader key %q",
					prev, cmd.ID, k)
			}
			leaderKeys[k] = cmd.ID
		}
		for _, mp := range cmd.Menu {
			if _, ok := c.nodeByID[mp.Parent]; !ok {
				return nil, fmt.Errorf("command %q is placed under unknown menu node %q",
					cmd.ID, mp.Parent)
			}
		}
	}
	// Leaf placements share the sibling namespace with child NODES, so the
	// collision check has to see both. Checking nodes against nodes only let a
	// command sit on the same order or hotkey as a submenu beside it, where one
	// of the two is unreachable by its key and their order is decided by luck.
	for i := range c.commands {
		cmd := &c.commands[i]
		if len(cmd.Menu) > 1 {
			return nil, fmt.Errorf("command %q declares %d menu placements; one "+
				"command has one place on the bar, or its rows collide on the id "+
				"they both carry", cmd.ID, len(cmd.Menu))
		}
		for _, mp := range cmd.Menu {
			s := sibs[mp.Parent]
			if s == nil {
				s = &sib{order: map[int]MenuNodeID{}, hotkey: map[rune]MenuNodeID{}}
				sibs[mp.Parent] = s
			}
			if prev, dup := s.order[mp.Order]; dup {
				return nil, fmt.Errorf("command %q and %q share order %d under %q",
					cmd.ID, prev, mp.Order, mp.Parent)
			}
			s.order[mp.Order] = MenuNodeID(cmd.ID)
			if mp.Hotkey != 0 {
				if prev, dup := s.hotkey[foldRune(mp.Hotkey)]; dup {
					return nil, fmt.Errorf("command %q and %q share hotkey %q under %q",
						cmd.ID, prev, mp.Hotkey, mp.Parent)
				}
				s.hotkey[foldRune(mp.Hotkey)] = MenuNodeID(cmd.ID)
			}
		}
	}

	return c, nil
}

// Command looks a command up by id. The second result is false for an unknown
// id, which is how a stale or forged action is refused rather than guessed at.
func (c *CatalogOf[H]) Command(id CommandID) (*CommandOf[H], bool) {
	cmd, ok := c.byID[id]
	return cmd, ok
}

// Commands enumerates every declared command, in declaration order.
func (c *CatalogOf[H]) Commands() []CommandOf[H] {
	return append([]CommandOf[H](nil), c.commands...)
}

// Nodes enumerates every declared menu node, in declaration order.
func (c *CatalogOf[H]) Nodes() []MenuNode { return append([]MenuNode(nil), c.nodes...) }

// leaderProjection returns the leader rows in Order — explicit, never inherited
// from map iteration or declaration accident.
//
// A DISABLED command is included, dimmed and carrying its reason, and its key
// is refused. Hiding it instead would make the menu shift under the operator
// and teach nothing about what the surface can do; closing the float on a dead
// key would read as though the command had run.
func (c *CatalogOf[H]) leaderProjection(h H) []leaderEntry {
	type row struct {
		order int
		entry leaderEntry
	}
	var rows []row
	for i := range c.commands {
		cmd := &c.commands[i]
		if cmd.Leader == nil {
			continue
		}
		off := cmd.offering(h)
		if off.State == OfferHidden {
			continue
		}
		label := cmd.Leader.text(h)
		id := cmd.ID
		if off.State == OfferDisabled {
			// The row stays, says why, and does nothing. Re-resolved at press
			// time rather than trusting this closure: the float can be open
			// while the state that disabled the row changes under it.
			// nil handler IS the disabled marker; see leaderEntry.
			label += " — " + off.Reason
			rows = append(rows, row{order: cmd.Leader.Order, entry: leaderEntry{
				key: cmd.Leader.Key, label: label, run: nil,
			}})
			continue
		}
		catalog := c
		rows = append(rows, row{order: cmd.Leader.Order, entry: leaderEntry{
			key: cmd.Leader.Key, label: label,
			run: func() { catalog.runIfOffered(h, id) },
		}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].order < rows[j].order })
	out := make([]leaderEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.entry)
	}
	return out
}

// HelpRow is one line of the help screen's command section.
type HelpRow struct {
	Key   rune
	Label string
	Help  string
}

// helpProjection is the leader commands as the help screen shows them.
//
// The SAME projection the leader menu executes, so the documented bindings
// cannot drift from the real ones — which is the property the old help screen
// had for most of its list and lost for the four it restated by hand.
func (c *CatalogOf[H]) helpProjection(h H) []HelpRow {
	type row struct {
		order int
		r     HelpRow
	}
	var rows []row
	for i := range c.commands {
		cmd := &c.commands[i]
		if cmd.Leader == nil || cmd.offering(h).State == OfferHidden {
			continue
		}
		rows = append(rows, row{order: cmd.Leader.Order, r: HelpRow{
			Key:   cmd.Leader.Key,
			Label: cmd.Leader.text(h),
			Help:  cmd.Leader.Help,
		}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].order < rows[j].order })
	out := make([]HelpRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.r)
	}
	return out
}

// runIfOffered is the ONE activation path, for every surface.
//
// It re-resolves immediately before running because a menu or a which-key float
// can stay open while an async transition invalidates the row under it. Not
// authorization — the server and the local capability checks remain that — but
// the menu's truth contract: what is offered is what happens.
func (c *CatalogOf[H]) runIfOffered(h H, id CommandID) bool {
	cmd, ok := c.byID[id]
	if !ok || !cmd.offered(h) {
		return false
	}
	cmd.Run(h)
	return true
}
