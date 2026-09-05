package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// The locked banner is the half ADR-0087 §6 RESTS ON.
//
// §6 keeps the daemon running through every keyfile failure, and justifies that
// by the state being loud and visible. If the banner is quiet, §6 is not
// honest — the daemon stays up in a degraded state that nobody can see, and the
// only symptom is every developer being refused.
//
// Its CONTENT is asserted rather than "it printed something", because a banner
// tested that way is a banner whose worst version passes.
func TestLockedBanner_SaysTheState_TheEffect_AndTheRemedy(t *testing.T) {
	t.Parallel()
	got := lockedBanner(errors.New("auth: service keyfile has unsafe permissions: /x/k is mode 0640"))

	for _, want := range []struct{ text, why string }{
		{"THE STORE IS LOCKED", "the state, in the first line an operator reads"},
		{"service keyslot did not open", "which mechanism failed, so they look in the right place"},
		{"mode 0640", "the REASON, passed through verbatim rather than summarised away"},
		{"IS RUNNING", "that the daemon did not die — otherwise they go looking for a crash"},
		{"57P03", "what clients are being told, so nobody debugs a credential problem"},
		{"NOT an authentication", "said explicitly, because that is the wrong turn this exists to prevent"},
		{"restart", "one of the two remedies"},
		{"passphrase", "the other remedy: unlock this process now without a restart"},
	} {
		if !strings.Contains(got, want.text) {
			t.Errorf("the banner does not carry %q — %s:\n%s", want.text, want.why, got)
		}
	}
}

// The REASON reaches the banner rather than being flattened into "the keyslot
// failed". §6's whole point is that the grounds are distinguishable from the
// log alone, and a banner that printed a generic line would throw that away at
// the last step.
func TestLockedBanner_CarriesEachGroundVerbatim(t *testing.T) {
	t.Parallel()
	for _, ground := range []error{
		auth.ErrKeyfileAbsent,
		auth.ErrKeyfileMode,
		auth.ErrKeyfileUnreadable,
		auth.ErrKeyfileMalformed,
		auth.ErrNoServiceKeyslot,
		auth.ErrKeyslotCorrupt,
	} {
		got := lockedBanner(ground)
		if !strings.Contains(got, ground.Error()) {
			t.Errorf("the banner does not carry %q; the five grounds §6 keeps the daemon "+
				"running for are indistinguishable at the one place an operator looks",
				ground.Error())
		}
	}
	// DECOY: two different grounds must not produce the same banner. A
	// implementation that dropped the reason would satisfy every assertion
	// above only by accident of substring matching, and this catches it.
	a := lockedBanner(auth.ErrKeyfileAbsent)
	b := lockedBanner(auth.ErrKeyfileMode)
	if a == b {
		t.Error("two different failure grounds produced an identical banner")
	}
}
