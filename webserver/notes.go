package webserver

import (
	"github.com/yongjohnlee80/autodb/core/config"
)

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
// validSubject delegates to the ONE canonical predicate. It used to live here and
// run only when a root was resolved — which is after login, after bootstrap, after
// the pool and after the ticket — so a configured subject of `../alice` reached
// the bootstrap path and became the permanent first admin before anything rejected
// it (lector r1 on PR #5). The rule now belongs to config, and is enforced at
// load, at New, and at admission.
func validSubject(s string) error { return config.ValidSubject(s) }

// refusalReason is the single browser-facing message for a subject the gateway
// will not admit.
//
// Deliberately NON-ENUMERATING and identical for every refusal: it reveals
// neither whether the daemon has been bootstrapped, nor who the bound subject
// is, nor whether the name exists. The operator learns which it was from the
// log; the browser learns only that it was refused (ADR-0064 §2.3).
const refusalReason = "webserver: the daemon refused the login"
