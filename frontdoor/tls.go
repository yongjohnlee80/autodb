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

	// SAN coverage of every configured name. verify-full verifies the NAME,
	// so a gap here fails at every client rather than at startup — one
	// connection at a time, with an error each developer reads as their own
	// client's problem.
	for _, host := range fd.TLSHostNames {
		if err := leaf.VerifyHostname(host); err != nil {
			return nil, fmt.Errorf("%w: the certificate does not cover %q (it carries %v) — "+
				"clients using sslmode=verify-full would each fail on their own, "+
				"reading it as a client-side problem: %w",
				ErrTLSMaterial, host, leaf.DNSNames, err)
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
