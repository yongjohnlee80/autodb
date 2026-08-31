// Package frontdoor implements autodb's PostgreSQL wire-protocol listener
// (ADR-0075), governed cell-by-cell by docs/front-door/protocol-matrix.md.
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
			return nil, fmt.Errorf("%w: the certificate does not verify for %q (it carries names %v) — "+
				"clients using sslmode=verify-full would each fail on their own, reading it as a "+
				"client-side problem. A missing intermediate looks exactly like this: %w",
				ErrTLSMaterial, host, leaf.DNSNames, verr)
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		// TLS 1.2 floor, 1.3 preferred (ADR-0075 §4, rev 2 MF3). The floor is
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
