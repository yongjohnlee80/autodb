package main

// A CONFIG THAT NAMES NO META STORE MAY NOT OPERATE ONE.
//
// The trap this closes was found on a live host. An installed front door puts
// two files in /etc/autodb: the server config, 0640 because it can name a
// PostgreSQL DSN with a password in it, and a world-readable client.toml that
// carries the daemon's address and NOTHING else. A developer who is not in the
// service's group can read only the second one, which is the point.
//
// `autodb --init` with no --config therefore resolved client.toml, and
// client.toml has no [meta] section by design -- so cfg.Meta fell back to the
// shipped default, a sqlite file in the CALLER's home. The ceremony then ran to
// completion against a private store: it created the first administrator,
// printed success, and left the service's own store empty. The operator had
// working credentials for a database nothing serves, and no indication that the
// store they had just bootstrapped was not the one the daemon reads.
//
// config.Server.ClientOnly already described this outcome -- "they get an empty
// store they could bootstrap themselves as administrator of" -- and the fix
// wired only the SPAWN seam, so the TUI could no longer start a daemon. The
// store seam was left open, and --init walked into the same end state through
// it. Naming a hazard is not guarding it.
//
// So the rule is a property of the FILE, checked once, in front of every entry
// point that opens a store from a resolved config: --init and --serve.
// --migrate-to-postgres takes its source and destination as explicit flags and
// never consults cfg.Meta, so it is not reachable this way; it is listed here
// because the comment on config.SystemPath claims it is, and that claim is
// wrong rather than load-bearing.

import (
	"fmt"
	"os"
	"strings"

	"github.com/yongjohnlee80/autodb/core/config"
)

// requireStoreConfig refuses to operate a meta store through a config that
// declares itself a client.
//
// `what` is the operation as a verb phrase and `remedy` the command line that
// does it properly; both are supplied by the caller so the refusal reads as an
// answer to what the operator actually ran.
func requireStoreConfig(cfg config.Config, what, remedy string) error {
	if !cfg.Server.ClientOnly {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "refusing to %s through a client config:\n       %s\n\n",
		what, describeSource(cfg))
	b.WriteString("This file sets client_only = true. It carries the daemon's address and\n" +
		"deliberately names no meta store, so proceeding would have opened a PRIVATE\n" +
		"store at\n")
	fmt.Fprintf(&b, "       %s\n", metaPathFor(cfg))
	b.WriteString("and treated it as this host's autodb -- leaving the store the service\n" +
		"actually reads untouched.\n\n")

	b.WriteString("Configs on this host:\n")
	for _, c := range storeConfigCandidates() {
		fmt.Fprintf(&b, "       %-44s %s\n", c.path, c.note)
	}

	fmt.Fprintf(&b, "\n%s\n", remedy)
	return fmt.Errorf("%s", b.String())
}

// describeSource names the file the refusal is about.
//
// ClientOnly cannot be true without a file having been read, so the fallback
// is unreachable today. It is here anyway because the alternative to an
// unreachable branch is a message that says "" -- and a refusal that names no
// file sends the operator to edit nothing.
func describeSource(cfg config.Config) string {
	if p := cfg.SourcePath(); p != "" {
		return p
	}
	return "(built-in defaults -- no config file was read)"
}

// configCandidate pairs a filesystem path with a diagnostic note about its status.
type configCandidate struct {
	path string
	note string
}

// storeConfigCandidates lists the files an operator could reasonably have
// meant, each annotated with what is actually true of it right now.
//
// The per-user config is ALWAYS reported, present or not: on a service host it
// is the file a developer would have to create to hold settings of their own,
// and resolution now prefers it over the installer's handout. Saying "absent"
// is more use than omitting it, because the absence is the actionable part.
func storeConfigCandidates() []configCandidate {
	out := []configCandidate{{path: config.SystemPath, note: annotate(config.SystemPath, "the service's own")}}
	if user, err := config.UserConfigPath(); err == nil {
		out = append(out, configCandidate{path: user, note: annotate(user, "your own")})
	}
	return out
}

// annotate reports presence and readability separately, because on a service
// host they differ and the difference is the whole explanation: the server
// config IS there, and you cannot read it, which is why resolution passed over
// it and handed you the client config instead.
func annotate(path, role string) string {
	if _, err := os.Stat(path); err != nil {
		return role + " (absent)"
	}
	f, err := os.Open(path)
	if err != nil {
		return role + " (present, not readable by you)"
	}
	_ = f.Close()
	return role + " (present)"
}
