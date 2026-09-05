package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/frontdoor"
)

// `autodb --create-cert` — the front door's TLS material in one command.
//
// The friction requirement is Johno's and it is the whole point: "TLS is
// important for encrypted communication, but we have to go about with least
// amount of friction." An operator who has to stand up a CA by hand turns TLS
// off instead, and ADR-0086 §10 is what that costs.
//
// NOT the production path. ADR-0075 prefers a real ACME certificate. This is
// for dev and internal deployments, and it says so in its own output rather
// than being gated — a second gate around a decision the operator already owns
// is the shape §10 considered and deliberately dropped.

// createCertOpts are the parsed flags.
type createCertOpts struct {
	// dir defaults to the config file's own directory, so the generated files
	// land next to the config that will name them.
	dir string
	// hosts overrides frontdoor.tls_host_names. Normally empty: taking the
	// names from live config is what makes the generated SANs and the startup
	// SAN check ONE list rather than two that agree today.
	hosts    []string
	leafOnly bool
	force    bool
	exportCA bool
	// now is injected for tests, exactly as LoadServerTLS takes a clock.
	now time.Time
}

// runCreateCert generates (or re-exports) the front door's TLS material.
func runCreateCert(out io.Writer, configPath string, o createCertOpts) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// Resolved through the package that owns the rule, not re-derived here.
	resolved, err := config.ResolvePath(configPath)
	if err != nil {
		return err
	}
	dir := o.dir
	if dir == "" {
		dir = defaultCertDir(resolved)
	}
	if o.exportCA {
		return exportCA(out, dir, cfg)
	}

	hosts := o.hosts
	if len(hosts) == 0 {
		hosts = cfg.FrontDoor.TLSHostNames
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no host names: set frontdoor.tls_host_names in %s, or pass "+
			"--cert-hosts. These are the names and addresses your developers will type into a "+
			"client, and sslmode=verify-full verifies the NAME — a certificate that omits one "+
			"fails at that client and nowhere else", resolved)
	}
	now := o.now
	if now.IsZero() {
		now = time.Now()
	}

	res, err := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: hosts, Now: now, LeafOnly: o.leafOnly, Force: o.force,
	})
	if err != nil {
		return err
	}
	report(out, res, resolved)
	return nil
}

// defaultCertDir puts the material beside the config that will point at it.
//
// A default at all is the friction argument; that it is DERIVED from the
// config path rather than a second hardcoded location is the resolver one —
// two places that each decide where autodb keeps its files is how they come to
// disagree ([[shared-resolver-single-source-of-truth]]).
func defaultCertDir(configPath string) string {
	if configPath == "" {
		return "tls"
	}
	return filepath.Join(filepath.Dir(configPath), "tls")
}

// report tells the operator what was made and what to do next.
//
// It prints the CONFIG BLOCK and the DSN rather than describing them, because
// the step after "certificate created" is transcription, and transcription is
// where the paths stop matching.
func report(out io.Writer, res frontdoor.CertResult, configPath string) {
	if res.ReusedCA {
		fmt.Fprintf(out, "Reissued the server certificate from the existing CA.\n")
		fmt.Fprintf(out, "  ca.pem is UNCHANGED — nobody needs a new copy.\n\n")
	} else {
		fmt.Fprintf(out, "Created a new CA and server certificate in %s\n\n", res.Dir)
	}
	fmt.Fprintf(out, "  %-16s %s\n", "ca.pem", res.CAFile)
	fmt.Fprintf(out, "  %-16s %s\n", "cert.pem", res.CertFile)
	fmt.Fprintf(out, "  %-16s %s\n", "key.pem", res.KeyFile)
	fmt.Fprintln(out)
	if len(res.DNSNames) > 0 {
		fmt.Fprintf(out, "  covers names      %s\n", strings.Join(res.DNSNames, ", "))
	}
	if len(res.IPAddresses) > 0 {
		fmt.Fprintf(out, "  covers addresses  %s\n", strings.Join(res.IPAddresses, ", "))
	}
	if len(res.Dropped) > 0 {
		// SAID OUT LOUD, because the alternative is a certificate that quietly
		// stopped covering an address and an operator whose own psql breaks
		// weeks later with nothing connecting the two events.
		fmt.Fprintf(out, "  NOT covered       %s\n", strings.Join(res.Dropped, ", "))
		fmt.Fprintf(out, "                    (this CA is name-constrained and cannot vouch for them;\n")
		fmt.Fprintf(out, "                     they are conveniences this command adds, not names you\n")
		fmt.Fprintf(out, "                     asked for, so the certificate was issued without them)\n")
	}
	fmt.Fprintf(out, "  certificate expires %s\n", res.LeafNotAfter.UTC().Format("2006-01-02"))
	if !res.ReusedCA {
		fmt.Fprintf(out, "  CA expires          %s\n", res.CANotAfter.UTC().Format("2006-01-02"))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Put this in %s:\n\n", nameOr(configPath, "your autodb config"))
	fmt.Fprintln(out, "  [frontdoor]")
	fmt.Fprintln(out, "  enabled = true")
	fmt.Fprintf(out, "  tls_cert_file = %q\n", res.CertFile)
	fmt.Fprintf(out, "  tls_key_file = %q\n", res.KeyFile)
	fmt.Fprintf(out, "  tls_root_ca_file = %q\n", res.CAFile)
	fmt.Fprintf(out, "  tls_host_names = [%s]\n", quoteList(append(append([]string{}, res.DNSNames...), res.IPAddresses...)))
	fmt.Fprintln(out)
	writeDistributionNotes(out, res)
}

// writeDistributionNotes states the boundary that decides whether this whole
// design holds.
//
// It is printed EVERY time rather than kept in documentation because the
// mistake it prevents — handing developers cert.pem and key.pem so "TLS works
// on their side too" — is one a helpful person makes while trying to reduce
// friction, which is the same instinct this command exists to serve.
func writeDistributionNotes(out io.Writer, res frontdoor.CertResult) {
	fmt.Fprintln(out, "Give every developer ONE file: ca.pem.")
	fmt.Fprintln(out, "  It is PUBLIC. Commit it internally, paste it in chat, mail it — it proves")
	fmt.Fprintln(out, "  nothing on its own and grants nothing.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  key.pem NEVER leaves this host. A developer holding it can impersonate")
	fmt.Fprintln(out, "  this front door and collect other developers' access tokens — the exact")
	fmt.Fprintln(out, "  attack mandatory TLS exists to prevent, arriving through the filesystem")
	fmt.Fprintln(out, "  instead of the network.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Their client settings, with ca.pem saved anywhere they like:\n\n")
	fmt.Fprintf(out, "  %s\n\n", sampleDSN(res))
	fmt.Fprintln(out, "This CA is name-constrained: it cannot issue a certificate for anything")
	fmt.Fprintln(out, "outside the names above, even if this host is compromised. That is why it")
	fmt.Fprintln(out, "must NOT be installed into a machine trust store — it does not need to be,")
	fmt.Fprintln(out, "and a private CA in the trust store is a standing risk that this one avoids.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "For a public, internet-facing deployment prefer a real ACME certificate")
	fmt.Fprintln(out, "(ADR-0075 §4). This command is for development and internal hosts.")
}

// sampleDSN is the line a developer pastes. It names the first covered
// address, which is what they will actually dial.
func sampleDSN(res frontdoor.CertResult) string {
	host := "<host>"
	switch {
	case len(res.DNSNames) > 0:
		host = res.DNSNames[0]
	case len(res.IPAddresses) > 0:
		host = res.IPAddresses[0]
	}
	return fmt.Sprintf("postgres://<user>:<token>@%s:5432/<database>?sslmode=verify-full&sslrootcert=/path/to/ca.pem", host)
}

// exportCA re-prints the developer-facing half without touching anything.
//
// It exists because the question "what do I send the new hire" is asked long
// after the certificate was created, and the honest answer is one file and one
// DSN — which is exactly what this prints.
func exportCA(out io.Writer, dir string, cfg config.Config) error {
	caFile := filepath.Join(dir, frontdoor.CAFileName)
	if cfg.FrontDoor.TLSRootCAFile != "" {
		// Prefer what the RUNNING configuration names over what this command's
		// default layout would guess: an operator who moved the file is
		// telling us where it is.
		caFile = cfg.FrontDoor.TLSRootCAFile
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w (run --create-cert first, or set "+
			"frontdoor.tls_root_ca_file if the CA lives elsewhere)", caFile, err)
	}
	fmt.Fprintf(out, "# %s — give this file to every developer. It is PUBLIC.\n", caFile)
	fmt.Fprintln(out, "# key.pem never leaves this host.")
	fmt.Fprintln(out)
	if _, err := out.Write(pem); err != nil {
		return err
	}
	fmt.Fprintln(out)
	host := "<host>"
	if len(cfg.FrontDoor.TLSHostNames) > 0 {
		host = cfg.FrontDoor.TLSHostNames[0]
	}
	fmt.Fprintf(out, "# postgres://<user>:<token>@%s:5432/<database>?sslmode=verify-full&sslrootcert=/path/to/ca.pem\n", host)
	return nil
}

func quoteList(items []string) string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, fmt.Sprintf("%q", i))
	}
	return strings.Join(out, ", ")
}

func nameOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// hostList is a repeatable/comma-separated --cert-hosts flag.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*h = append(*h, p)
		}
	}
	if len(*h) == 0 {
		return errors.New("empty host name")
	}
	return nil
}
