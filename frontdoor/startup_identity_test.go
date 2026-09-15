package frontdoor

import (
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// TWO REFUSALS, TWO IDENTITIES, TWO CHARGES.
//
// A startup parameter can be refused twice over, and the two refusals are not
// the same event:
//
//   - BEFORE any credential, by this package, when the startup packet names a
//     parameter the surface does not accept. Nobody has proved anything yet,
//     and probing the startup surface repeatedly is how an attacker maps it —
//     so it is charged as protocol grinding.
//
//   - AFTER a PAT has been verified, by the engine, when an accepted parameter
//     names a setting this session may not change. The caller has already
//     proved who they are; what they met is a fact about our configuration,
//     and charging it means a developer whose client sets `datestyle` bans
//     themselves by reconnecting.
//
// They shared one audit string until this was split. An operator counting that
// string was counting both, and the engine's own comment claimed they were
// distinct identities while the value said otherwise — a comment describing a
// contract the code did not keep.
//
// This cell is the thing that stops them merging again. It lives here because
// this is the package that can see both.
func TestStartupRefusals_ThePreAndPostVerificationIdentitiesAreDistinct(t *testing.T) {
	t.Parallel()

	pre := string(reasonStartupParamRefus)
	post := exec.DenyStartupGUC

	if pre == post {
		t.Fatalf("both refusals audit as %q. One fires before any credential and one only "+
			"after a verified PAT; they cost the peer different things, and a single string "+
			"makes the difference unreadable in the trail", pre)
	}

	// AND THE CHARGES DIFFER, which is why the identities must. The engine
	// classifies its own; the front door charges the pre-verification refusal
	// unconditionally, in the branch that handles a refused startup.
	class, ok := exec.DenialCharge(post)
	if !ok {
		t.Fatalf("%q has no ruled charge class", post)
	}
	if class.Charges() {
		t.Errorf("the post-verification refusal is charged as %s. It follows a verified "+
			"credential, so a developer whose client sets a setting the session may not "+
			"would ban themselves by reconnecting", class)
	}
	// The pre-verification one must NOT be classified by the engine at all: it
	// is not the engine's outcome, and a class registered for it here would be
	// the merge this cell exists to prevent, arriving from the other side.
	if _, engineOwns := exec.DenialCharge(pre); engineOwns {
		t.Errorf("the engine classifies %q, which is the front door's pre-verification "+
			"refusal; the two identities are merging again", pre)
	}
}
