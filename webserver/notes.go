package webserver

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yongjohnlee80/autodb/core/config"
)

// maxSubjectLen bounds the directory component built from a username. Generous
// for a person's login name, far short of any filesystem limit.
const maxSubjectLen = 64

// noteRootFor returns the note root for one authenticated user.
//
// # Why per user at all
//
// tuiapp.NewNoteStore takes one immutable root and lists every ws-* directory
// under it, with no reference to who is asking — notes are read from disk, and
// disk has no identity. One process now serves N users, so a single shared root
// would hand every web user every other web user's notes. The terminal frontend
// never had this problem because the OS gave it one user per process.
//
// # Why the subject is not simply interpolated
//
// Because it becomes a path component, and a name that reaches a filesystem path
// should not depend on the daemon being well-behaved. The subject comes from the
// daemon's answer to auth.login and never from a request field, so this is not
// guarding against a hostile client — it is refusing to make the daemon's input
// validation load-bearing for autodb's filesystem layout (ADR-0061 §2.8, lector
// r2). A separator or a `..` here is the difference between a note directory and
// someone else's home.
//
// Rejected rather than sanitised: a name that has to be rewritten to be safe is a
// name whose owner should be told, not silently given a different directory than
// the one their username implies. Two users whose names sanitise to the same
// string would otherwise share notes.
// noteRootForMode resolves the root for one session under the configured mode
// (ADR-0064 §2.3).
//
// workspace mode returns the SHARED base — the tree the terminal TUI writes,
// containing ws-* directories — and is only reachable for the bound subject,
// because Gateway.subjectAllowed refused everyone else before any session
// existed. per-user mode keeps ADR-0061 §2.8's isolation unchanged.
func noteRootForMode(base, subject string, mode config.NotesMode) (string, error) {
	if mode == config.NotesWorkspace {
		// Still validated: the subject is not interpolated into a path here, but a
		// caller reaching this with an unsafe name means an admission bug upstream,
		// and failing loudly beats reading the shared tree on its behalf.
		if err := validSubject(subject); err != nil {
			return "", err
		}
		return base, nil
	}
	return noteRootFor(base, subject)
}

func noteRootFor(base, subject string) (string, error) {
	if err := validSubject(subject); err != nil {
		return "", err
	}
	return filepath.Join(base, "u-"+subject), nil
}

func validSubject(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("webserver: empty subject cannot name a note directory")
	case len(s) > maxSubjectLen:
		return fmt.Errorf("webserver: subject is %d bytes, over the %d-byte limit for "+
			"a note directory", len(s), maxSubjectLen)
	case s == "." || s == "..":
		return fmt.Errorf("webserver: subject %q is a path traversal", s)
	case strings.ContainsAny(s, `/\`):
		return fmt.Errorf("webserver: subject %q contains a path separator", s)
	case strings.HasPrefix(s, "."):
		// A leading dot would hide the directory and, worse, `..anything` reads as
		// traversal to a human scanning a listing.
		return fmt.Errorf("webserver: subject %q starts with a dot", s)
	}
	// A conservative allowlist, not a denylist: the set of characters that break a
	// path is longer than the set a username needs, and only one of those lists
	// can be written down completely.
	for _, r := range s {
		ok := r == '-' || r == '_' || r == '.' || r == '@' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("webserver: subject %q contains %q, which is not allowed "+
				"in a note directory name", s, r)
		}
	}
	return nil
}

// refusalReason is the single browser-facing message for a subject the gateway
// will not admit.
//
// Deliberately NON-ENUMERATING and identical for every refusal: it reveals
// neither whether the daemon has been bootstrapped, nor who the bound subject
// is, nor whether the name exists. The operator learns which it was from the
// log; the browser learns only that it was refused (ADR-0064 §2.3).
const refusalReason = "webserver: the daemon refused the login"
