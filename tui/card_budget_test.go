package tui

import (
	"strings"
	"testing"
)

// budgetEndpoint is liveEndpoint with the ceilings a real daemon reports.
// budgetConn is a connection that carries its own bound, which is the number
// that actually holds a client to something.
func budgetConn() ConnInfo {
	return ConnInfo{ID: 1, Name: "c", TargetDB: "db", PoolMaxConns: 6}
}

func budgetEndpoint() FrontDoorEndpoint {
	ep := liveEndpoint()
	ep.MaxSessionsPerUser = 8
	ep.MaxSessionsGlobal = 256
	ep.MaxTargetConns = 25
	return ep
}

// THE CEILINGS ARE SHOWN, AND SO IS WHOSE THEY ARE.
//
// The number alone is the trap: a developer reading "sessions per user 8"
// without its scope reads it as an allowance, and two of them behind one
// address then ask for sixteen. Naming the scope beside each figure is the
// whole point of showing it, so the scope is asserted with the number rather
// than left to the layout.
func TestCardBudget_TheCeilingsCarryTheirScope(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")

	for _, want := range []struct{ figure, scope string }{
		{"8", "across every database"},
		{"256", "everyone using this autodb"},
		{"25", "everyone, across every target"},
	} {
		line := lineContaining(t, text, want.scope)
		if !strings.Contains(line, want.figure) {
			t.Errorf("the row scoped %q does not carry its figure %q: %q",
				want.scope, want.figure, line)
		}
	}

	if !strings.Contains(text, "They are ceilings, and they are SHARED") &&
		!strings.Contains(text, "are ceilings, and they are SHARED") {
		t.Error("the card does not say the ceilings are shared; a developer reading " +
			"them as an allowance is how two processes come to ask for twice the cap")
	}
}

// THE CARD DOES NO ARITHMETIC AT ALL.
//
// It used to state the demand as a formula and give a worked example, on the
// reasoning that a shared ceiling cannot yield a private number so the reader
// should compute their own. Johno's ruling removes the premise: "Each conn
// should have allowed MAX CONNS to deal with anyways." There is nothing for a
// developer to compute, so a formula on this card is an invitation to tune
// something autodb already holds them to.
//
// Three earlier attempts at that arithmetic were each wrong in the same way,
// and each looked perfectly reasonable in a diff -- which is why they are
// listed here by their own words rather than left to a reviewer to recognise
// the fourth.
func TestCardBudget_ItDoesNoArithmetic(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")
	lower := strings.ToLower(text)

	for _, dead := range []struct{ phrase, why string }{
		{"idle should equal open",
			"holding every connection open hoards leases other people are waiting for"},
		{"allotted share",
			"nothing allocates a share; admission is first-come-first-served with no per-user slices"},
		{"divided by the number of databases",
			"that hands the entire shared cap to every process, so two processes ask for twice it"},
		{"processes x databases",
			"the demand formula asks a developer to compute a number they no longer need"},
		{"worked example",
			"an example of pool sizing is pool-sizing advice with a disclaimer attached"},
	} {
		if strings.Contains(lower, dead.phrase) {
			t.Errorf("the card says %q again: %s", dead.phrase, dead.why)
		}
	}
}

// THE PER-SOURCE ROW IS ABSENT, NOT APPROXIMATED.
//
// What autodb has per source today is a credential and TLS failure count in a
// window, plus the temporary throttle that follows — a rate limit on FAILURES.
// It is not a ceiling on concurrent capacity, and showing it as one would
// teach a developer the wrong model of what is limiting them. That is the
// exact confusion the incident behind this work was made of: capacity
// exhaustion read as a credential problem.
//
// The cell is here because the row is easy to add and looks like an
// improvement.
func TestCardBudget_NoPerSourceConcurrencyCapIsAdvertised(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")
	lower := strings.ToLower(text)

	for _, claim := range []string{"per source", "per-source", "from your address", "per address"} {
		if strings.Contains(lower, claim) {
			t.Errorf("the card advertises a per-source limit (%q). autodb has no "+
				"concurrent per-source cap; what it has is a failure-rate throttle, and "+
				"presenting that as a concurrency ceiling teaches the wrong model of "+
				"what is limiting somebody", claim)
		}
	}
}

// A FIGURE THIS DAEMON DID NOT REPORT IS NOT A CAP OF ZERO.
//
// An older daemon answering a newer frontend sends nothing for these fields.
// Rendering the zero would tell a developer they may open no sessions at all —
// a confident wrong answer where the honest one is that this daemon does not
// say. The failure is silent and total, and it arrives exactly when someone
// upgrades their frontend and not the shared daemon, which is the normal case.
func TestCardBudget_AnUnreportedCeilingSaysSoRatherThanSayingZero(t *testing.T) {
	ep := liveEndpoint() // an older daemon: no ceilings in the reply
	text, _ := buildCardText("tok", budgetConn(), ep, "johno", "editor", "")

	line := lineContaining(t, text, "sessions per user")
	if !strings.Contains(line, "not reported") {
		t.Errorf("an unreported ceiling renders as %q; it must say the daemon did not "+
			"report it, because a printed zero reads as a cap of zero",
			strings.TrimSpace(line))
	}
	if strings.Contains(line, " 0 ") {
		t.Error("the card printed a cap of zero for a figure nobody sent")
	}
}

// THE CARD TELLS NOBODY TO CONFIGURE ANYTHING.
//
// Johno's ruling: "we won't enforce PG_MAX_OPEN_CONNS" and "Each conn should
// have allowed MAX CONNS to deal with anyways." An earlier version of this
// block carried per-client pool recipes, one per client, each correct for its
// client -- and every one of them asked a developer to know a number, which the
// acceptance rules retire outright. This cell holds the recipes out by name,
// because they are easy to re-add and each one looks like a helpful detail.
func TestCardBudget_ItTellsNobodyToConfigureAPool(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")

	for _, setting := range []string{
		"PG_MAX_OPEN_CONNS", "PG_MAX_IDLE_CONNS",
		"SetMaxOpenConns", "SetMaxIdleConns",
		"maximumPoolSize", "minimumIdle",
		"HikariCP", "DataGrip",
	} {
		if strings.Contains(text, setting) {
			t.Errorf("the card tells somebody to set %q. A developer points an "+
				"application at the front door and it works; a card handing out sizing "+
				"advice teaches the thing the scheduler exists to stop them needing",
				setting)
		}
	}

	// And it says so positively, so the absence reads as a decision rather
	// than an omission somebody should fill in.
	if !strings.Contains(text, "You do not size anything") {
		t.Error("the card does not say the limits are enforced regardless of the " +
			"client's own configuration, so a reader is left to assume they must " +
			"still tune something")
	}
}

// THE BOUND THAT ACTUALLY DEALS WITH IT IS THE ONE SHOWN FIRST.
//
// Each connection carries its own ceiling on pooled connections, and autodb
// holds a client to it whether or not the client sized itself. That is the
// number governing this token most directly, so it leads the list rather than
// sitting under three instance-wide figures.
func TestCardBudget_TheConnectionsOwnBoundLeads(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")

	line := lineContaining(t, text, "this connection")
	if !strings.Contains(line, "6") {
		t.Errorf("the connection's own bound is not shown with its figure: %q", line)
	}
	own := strings.Index(text, "this connection")
	perUser := strings.Index(text, "sessions per user")
	if own < 0 || perUser < 0 {
		t.Fatalf("fixture rendered neither row (own=%d perUser=%d)", own, perUser)
	}
	if own > perUser {
		t.Error("the connection's own bound is listed below the instance-wide figures; " +
			"it is the one that governs this token most directly")
	}

	// A connection that sets none of its own says nothing rather than zero.
	plain := budgetConn()
	plain.PoolMaxConns = 0
	bare, _ := buildCardText("tok", plain, budgetEndpoint(), "johno", "editor", "")
	if strings.Contains(bare, "this connection") {
		t.Error("a connection with no bound of its own still got a row; the engine's " +
			"ceiling applies there and the card would be inventing a number")
	}
}

// WHAT IS NOT HERE, AND WHY.
//
// A cell asserting that the copy key still yields the token alone was written,
// passed, and was then DELETED. Two things were wrong with it. It handed
// cardCopyKeys a DSN rather than the value production passes, so it asserted a
// property of its own argument -- the same shape as the defects this milestone
// keeps turning up. And the contract it named is not this card's: the design
// says "the copy keys still yield the token alone", but `Y` was since changed
// to copy the WHOLE card deliberately, because a token-only `y` claimed
// the key before the read-only editor beneath could treat it as a yank, which
// made a visual selection uncopyable.
//
// The design's intent -- that a screen of prose must never land in a password
// field -- is met by `y` staying an ordinary selection yank and `Y` being
// labelled for what it takes. Both are already asserted, on the real binding
// tables, in TestCard_CopyKeysAreYankFriendly and
// TestCard_FooterNamesTheAvailableKeys. Adding a third cell that reasserted
// them from arguments of its own would have manufactured the appearance of
// coverage for a divergence that needs a ruling instead, and the divergence is
// raised rather than absorbed.

// NOTHING ON THE CARD IS A LIVE FIGURE.
//
// The card is shown once and cannot be recovered, so a count of what is free
// right now is stale before it is read and misleading afterwards. Live figures
// belong in the pressure view, which carries a timestamp saying when it was
// true.
func TestCardBudget_ItShowsNoLiveAvailability(t *testing.T) {
	text, _ := buildCardText("tok", budgetConn(), budgetEndpoint(), "johno", "editor", "")
	lower := strings.ToLower(text)

	for _, live := range []string{"currently", "right now", "available now", "in use", "free now"} {
		if strings.Contains(lower, live) {
			t.Errorf("the card carries what reads as a live figure (%q). It is displayed "+
				"once and never recoverable, so any such number is stale before it is "+
				"read; live figures belong in the pressure view with a timestamp", live)
		}
	}
}

// AND A CARD FOR AN UNUSABLE TOKEN DOES NOT LECTURE ABOUT POOL SIZES.
//
// The top of the card already says the token cannot be used anywhere. Pool
// guidance beneath that is noise between the reader and the one thing they
// need to act on.
func TestCardBudget_AnUnusableTokenGetsNoBudgetBlock(t *testing.T) {
	text, _ := buildCardText("tok", ConnInfo{ID: 1, Name: "c", TargetDB: "db"},
		FrontDoorEndpoint{Enabled: false}, "johno", "editor", "")

	if strings.Contains(text, "LIMITS THAT APPLY") || strings.Contains(text, "PG_MAX_OPEN_CONNS") {
		t.Error("a token that cannot be used anywhere still got pool-sizing advice, " +
			"between the reader and the warning that is the only thing to act on")
	}
}

// lineContaining returns the one line holding needle, failing if there is not
// exactly one — an assertion made against a whole screen matches things the
// author never meant, which has already cost this package a cell that tripped
// on "connections" while looking for "ns".
func lineContaining(t *testing.T, text, needle string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, needle) {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 0:
		t.Fatalf("no line on the card contains %q", needle)
	case 1:
		return found[0]
	}
	t.Fatalf("%d lines contain %q, so an assertion about \"the\" line is ambiguous: %q",
		len(found), needle, found)
	return ""
}
