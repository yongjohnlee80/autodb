// Package frontdoor implements autodb's PostgreSQL wire-protocol listener
// governed cell-by-cell by docs/front-door/protocol-matrix.md.
package frontdoor

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
)

// ErrTLSMaterial reports server TLS material this listener will not serve
// with. It is a start-up refusal, never a per-connection one.
var ErrTLSMaterial = errors.New("frontdoor: refusing to serve with this TLS material")

// LoadServerTLS validates the configured certificate and key and returns the
// listener's TLS configuration.
//
// This runs BEFORE bind/listen, and that ordering is the point (protocol
// matrix row 2.1b). A front door that binds first and discovers its identity
// is unusable later has already accepted connections it must then fail — and
// on this surface a connection is a client presenting an access token, so
// failing late means having asked for a credential the listener was never in
// a position to protect. The daemon does not start rather than listen with an
// identity it cannot prove.
//
// Every rejection below is a real deployment mistake rather than a
// hypothetical: a path typo, an expired certificate nobody was watching, a
// renewed leaf pasted next to last quarter's key, a chain missing its
// intermediate, or a certificate that simply does not carry the name the
// clients dial. Each is reported as itself, because "TLS error" sends an
// operator to inspect the wrong thing.
func LoadServerTLS(fd config.FrontDoor, now time.Time) (*tls.Config, error) {
	certPEM, err := os.ReadFile(fd.TLSCertFile)
	if err != nil {
		return nil, fmt.Errorf("%w: reading tls_cert_file: %w", ErrTLSMaterial, err)
	}
	keyPEM, err := os.ReadFile(fd.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%w: reading tls_key_file: %w", ErrTLSMaterial, err)
	}

	// X509KeyPair is also the key/cert MATCH check: it derives the public key
	// from the private one and compares. A renewed leaf sitting next to the
	// previous key fails here and nowhere else useful.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: the certificate and key do not form a usable pair "+
			"(a renewed certificate left beside the previous key looks exactly like this): %w",
			ErrTLSMaterial, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("%w: parsing the leaf certificate: %w", ErrTLSMaterial, err)
	}
	pair.Leaf = leaf

	// Validity is checked against a passed-in clock rather than time.Now so
	// the expiry and not-yet-valid paths are testable without waiting for a
	// certificate to age.
	switch {
	case now.Before(leaf.NotBefore):
		return nil, fmt.Errorf("%w: the certificate is not valid until %s (the clock here says %s) — "+
			"a certificate from the future is usually a clock problem on this host, not a bad file",
			ErrTLSMaterial, leaf.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		return nil, fmt.Errorf("%w: the certificate expired at %s; renewal did not reach this host",
			ErrTLSMaterial, leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	// The CHAIN, not just the leaf.
	//
	// The first version of this checked the leaf's own fields and stopped —
	// key pair, validity, VerifyHostname — and called that "wrongly-chained
	// material fails startup", which it was not. A leaf served without its
	// intermediate parses perfectly, is in date, and carries the right name;
	// it simply cannot be built into a path to any root, so every verifying
	// client rejects it and the listener is the last to know. That is the
	// same failure the SAN check exists to prevent, one level up, and it is
	// the ordinary shape of a real misconfiguration: a renewal that wrote
	// cert.pem where fullchain.pem was meant.
	//
	// Intermediates come from the certificate file itself (everything after
	// the leaf, as a PEM bundle conventionally is). Roots come from the
	// configured CA when there is one and the system's otherwise, which is
	// what lets the ADR's two sanctioned cases — public ACME, or a securely
	// distributed private CA — both verify here.
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, perr := x509.ParseCertificate(der)
		if perr != nil {
			return nil, fmt.Errorf("%w: parsing an intermediate certificate: %w", ErrTLSMaterial, perr)
		}
		intermediates.AddCert(c)
	}
	roots, err := trustRoots(fd.TLSRootCAFile)
	if err != nil {
		return nil, err
	}

	// One Verify per configured name. Verify subsumes the SAN check, the
	// chain, and server-auth key usage — but it is run per name so the
	// message can say WHICH name failed; a combined check would report only
	// that something did.
	for _, host := range fd.TLSHostNames {
		if _, verr := leaf.Verify(x509.VerifyOptions{
			DNSName:       host,
			Intermediates: intermediates,
			Roots:         roots,
			CurrentTime:   now,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); verr != nil {
			return nil, fmt.Errorf("%w: %s", ErrTLSMaterial,
				describeVerifyFailure(host, certIdentities(leaf), fd.TLSRootCAFile, verr))
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		// TLS 1.2 floor, 1.3 preferred. The floor is
		// a minimum and not a target: everything below it is broken in public.
		MinVersion: tls.VersionTLS12,
		// PostgreSQL 17's direct-TLS negotiation advertises ALPN
		// "postgresql". v1 refuses direct TLS (matrix row 2.1a) so that there
		// is ONE negotiation path to test, and advertising nothing here is
		// part of refusing it.
		NextProtos: nil,
	}, nil
}

// trustRoots is the root pool the server's own chain is verified against:
// the configured CA bundle when there is one, the host's system roots
// otherwise.
//
// An unreadable or unparsable CA file is a start-up refusal rather than a
// silent fall back to system roots. Falling back would turn "my private CA
// path has a typo" into "verification passes against roots that were never
// meant to sign this", which is a weaker check wearing the same green.
// trustSource names the store the verification actually consulted.
//
// A FACT ABOUT THIS PROCESS'S CONFIGURATION, and the one thing an operator can
// act on when a chain fails. The old message never said it, so a private-CA
// install that had forgotten tls_root_ca_file looked identical to one whose
// certificate was simply wrong.
func trustSource(caFile string) string {
	if caFile == "" {
		return "the host's system roots (no [frontdoor] tls_root_ca_file configured)"
	}
	return "tls_root_ca_file " + caFile
}

// certIdentities is every identity a certificate carries: DNS names AND IP
// addresses, in one display.
//
// leaf.DNSNames alone was the whole set for as long as autodb issued DNS SANs
// only. --create-cert takes addresses too and puts them in IPAddresses, where
// they belong -- so the message reported a name set that was incomplete, and
// for an IP-only certificate reported "carries []" while the x509 error
// appended two lines later listed the addresses it actually covers. A message
// that contradicts its own evidence is worse than a terse one: review's probe
// configured 192.0.2.11 against a certificate for 192.0.2.10 and the headline
// blamed a DNS name that had nothing to do with it.
//
// One function, used at the one call site, so the two branches that print
// identities cannot drift apart.
func certIdentities(leaf *x509.Certificate) []string {
	out := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	out = append(out, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		// net.IP.String canonicalises, which is the form an operator sees in
		// the x509 error and in their own config.
		out = append(out, ip.String())
	}
	return out
}

// describeVerifyFailure turns one x509 verification failure into a message
// about the RIGHT SUBJECT.
//
// leaf.Verify subsumes three different failures — the SAN check, the chain, and
// server-auth key usage — and all three used to be formatted around the leaf's
// DNSNames. That is correct for the first and misleading for the other two: on
// a real bring-up it sent the operator to inspect SANs that were fine while the
// trust root was unset, through sixty restarts.
//
// THE BRANCH COMES FROM THE ERROR'S TYPE, via errors.As, never from matching
// its text. And the default branch is LABELLED rather than quietly folded into
// one of the others: if a future Go changes these shapes, "unclassified" tells
// the next reader the classification did not fire, where a confident wrong
// branch would not.
func describeVerifyFailure(host string, identities []string, caFile string, verr error) string {
	// VALUE TARGETS, not pointers. crypto/x509 returns these errors BY VALUE,
	// so errors.As against *x509.HostnameError matches nothing — my first
	// version did exactly that and every real failure fell through to the
	// unclassified branch. Which is the branch doing its job: it reported that
	// the classification had not fired instead of picking a confident wrong
	// answer, and that is what the label is for.
	var (
		nameErr  x509.HostnameError
		authErr  x509.UnknownAuthorityError
		validErr x509.CertificateInvalidError
	)
	switch {
	case errors.As(verr, &nameErr):
		// THE IDENTITIES ARE THE ANSWER HERE, and nothing else is implicated: the
		// chain verified, so naming the trust root would misdirect.
		return fmt.Sprintf("the certificate does not cover the configured name %q — it "+
			"carries %v. Either add %q to the certificate (autodb --create-cert "+
			"reads frontdoor.tls_host_names) or configure a name it already covers. "+
			"Clients using sslmode=verify-full would each fail on their own, reading it "+
			"as a client-side problem: %v", host, identities, host, verr)

	case errors.As(verr, &authErr):
		// LEAD WITH THE TRUST SOURCE. The SANs come last, because they are
		// still useful and are not the headline.
		//
		// The unset root is offered as the FIRST THING TO CHECK, never as the
		// cause: it does not prove an intermediate is missing, and it does not
		// prove one is present. A chain can fail against system roots for
		// several reasons, and a leaf whose intermediate really is absent
		// fails the same way with the root configured.
		lead := fmt.Sprintf("the certificate chain does not verify against %s",
			trustSource(caFile))
		checkFirst := "check that tls_cert_file is the FULL chain (leaf plus any " +
			"intermediates), not the leaf alone"
		if caFile == "" {
			checkFirst = "on a private-CA install, the first thing to check is that " +
				"[frontdoor] tls_root_ca_file names your CA — it is unset, so nothing " +
				"but the system roots was trusted"
		}
		return fmt.Sprintf("%s.\n  %s.\n  The certificate itself is for %v, which is not "+
			"what failed here: %v", lead, checkFirst, identities, verr)

	case errors.As(verr, &validErr) && validErr.Reason == x509.IncompatibleUsage:
		// A CERTIFICATE FOR A DIFFERENT JOB. Neither a name nor a chain
		// problem, and the old message described it as both.
		return fmt.Sprintf("the certificate is not valid for server authentication — it "+
			"carries extended key usages that exclude it, so it cannot serve TLS whatever "+
			"its names or chain are: %v", verr)

	default:
		// LABELLED, and carrying everything, because nothing narrowed it.
		return fmt.Sprintf("the certificate does not verify for %q, and the failure is "+
			"unclassified — none of the known shapes (name mismatch, unknown authority, "+
			"key usage) matched, so this reports what happened rather than guessing why. "+
			"Trust source: %s. Certificate identities: %v. Verification error: %v",
			host, trustSource(caFile), identities, verr)
	}
}

// trustRoots loads an x509.CertPool containing trust roots from caFile, or falls back to system roots.
func trustRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("%w: reading the system trust roots (set frontdoor.tls_root_ca_file "+
				"to name a CA bundle explicitly): %w", ErrTLSMaterial, err)
		}
		return pool, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("%w: reading tls_root_ca_file: %w", ErrTLSMaterial, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: tls_root_ca_file %s contains no usable certificate",
			ErrTLSMaterial, caFile)
	}
	return pool, nil
}
