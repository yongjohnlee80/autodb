package frontdoor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Certificate generation for the front door (ADR-0075 §4, ADR-0086 §10).
//
// It lives beside LoadServerTLS ON PURPOSE. The rules for what a certificate
// must carry and the rules for what is accepted at startup are the same rules,
// and in two packages they drift — the SAN set generated here is exactly the
// SAN set verified there ([[shared-resolver-single-source-of-truth]]).
//
// This is NOT the production path. ADR-0075 prefers a real ACME certificate;
// this exists so a dev or internal deployment is not forced to choose between
// standing up a CA by hand and turning TLS off. Saying so is a documentation
// job and deliberately not another gate.
//
// THREE FINDINGS FROM MEASUREMENT SHAPE EVERY DECISION BELOW. Each was
// verified against pgjdbc 42.7.3 (the driver JetBrains ships, so it is what
// DataGrip runs), libpq/OpenSSL via psql, and Go's own verifier — the three
// stacks a client of this front door can be using. They are recorded here
// because each looks like an arbitrary choice and each is load-bearing.
//
//  1. pgjdbc DOES match iPAddress SANs. AlloyDB has no DNS name and the
//     autodb host is not required to have one either, so the leaf carries the
//     bind address as an IP SAN. This was an unverified assumption in the ADR
//     and is now measured: a leaf carrying ONLY IP:127.0.0.1 verified, and a
//     leaf carrying only IP:10.99.99.99 was rejected at the same address —
//     the control that proves the verifier was running at all.
//
//  2. A NAME-CONSTRAINED SELF-SIGNED ROOT IS NOT ENFORCED BY JAVA. Java
//     applies name constraints to certificates IN the path; a trust anchor's
//     own certificate is not one. Measured: a leaf whose address was OUTSIDE
//     its root's PermittedIPRanges was rejected by psql and Go and ACCEPTED by
//     pgjdbc. So the constraint lives on an INTERMEDIATE, which is in the
//     path, and all three stacks then enforce it.
//
//  3. JAVA CHECKS THE LEAF'S CommonName AS IF IT WERE A DNS NAME whenever a
//     dNSName constraint is present. Go and OpenSSL do not. A leaf with
//     CN=some-host under a CA permitting only autodb.example.com was rejected
//     by pgjdbc and accepted by psql and Go — TLS that works in psql and fails
//     in DataGrip, with an error pointing nowhere useful. So the leaf carries
//     NO CommonName at all. Verified in the same batch as its control: the
//     identical leaf with a CN failed, and with the CN removed it passed.

// ErrCertMaterial reports a refusal to generate. Like ErrTLSMaterial it is
// raised before anything is written, never half way through.
var ErrCertMaterial = errors.New("frontdoor: refusing to generate this TLS material")

// Certificate file names inside the output directory.
//
// Named as constants because three of them are referenced by config keys and
// one is handed to every developer; a rename is a documentation event.
const (
	CAFileName     = "ca.pem"           // the trust anchor. PUBLIC — this is the file devs receive.
	CAKeyFileName  = "ca.key"           // signs the intermediate. Never leaves this host.
	IntFileName    = "intermediate.pem" // name-constrained, bundled into cert.pem
	IntKeyFileName = "intermediate.key"
	CertFileName   = "cert.pem" // leaf + intermediate, in that order (tls_cert_file)
	KeyFileName    = "key.pem"  // the leaf's key (tls_key_file)
)

// CertRequest is what to mint.
type CertRequest struct {
	// Dir receives the files. Created 0700 if missing.
	Dir string
	// HostNames are the names and addresses clients will dial — normally
	// frontdoor.tls_host_names verbatim, so that what is generated and what
	// LoadServerTLS checks come from ONE list.
	HostNames []string
	// Now is the clock. Passed in so validity windows are testable without
	// waiting for a certificate to age, exactly as LoadServerTLS takes one.
	Now time.Time
	// CALifetime and LeafLifetime default to 10 years and 1 year.
	CALifetime, LeafLifetime time.Duration
	// LeafOnly reissues from the existing CA and intermediate, leaving both
	// untouched — the rotation case, and the only one that does not require
	// redistributing ca.pem.
	LeafOnly bool
	// Force permits overwriting existing key material.
	Force bool
}

// CertResult reports what was written.
type CertResult struct {
	Dir                       string
	CAFile, CertFile, KeyFile string
	DNSNames                  []string
	IPAddresses               []string
	LeafNotAfter              time.Time
	CANotAfter                time.Time
	ReusedCA                  bool
}

const (
	defaultCALifetime   = 10 * 365 * 24 * time.Hour
	defaultLeafLifetime = 365 * 24 * time.Hour
)

// CreateCert generates the front door's TLS material.
func CreateCert(req CertRequest) (CertResult, error) {
	if strings.TrimSpace(req.Dir) == "" {
		return CertResult{}, fmt.Errorf("%w: no output directory", ErrCertMaterial)
	}
	dns, ips, err := splitHostNames(req.HostNames)
	if err != nil {
		return CertResult{}, err
	}
	if req.CALifetime <= 0 {
		req.CALifetime = defaultCALifetime
	}
	if req.LeafLifetime <= 0 {
		req.LeafLifetime = defaultLeafLifetime
	}
	if err := os.MkdirAll(req.Dir, 0o700); err != nil {
		return CertResult{}, fmt.Errorf("%w: creating %s: %w", ErrCertMaterial, req.Dir, err)
	}

	p := func(name string) string { return filepath.Join(req.Dir, name) }

	var (
		caCert, intCert *x509.Certificate
		intKey          *ecdsa.PrivateKey
		reused          bool
	)
	if req.LeafOnly {
		caCert, intCert, intKey, err = loadIssuer(req.Dir)
		if err != nil {
			return CertResult{}, err
		}
		reused = true
		// REFUSE HERE rather than emit a leaf every client rejects. A
		// name-constrained CA cannot vouch for an address outside its
		// constraint, and x509.CreateCertificate will sign one anyway — the
		// failure would surface at each client as an unexplained verification
		// error, which is the exact shape LoadServerTLS's startup checks exist
		// to prevent.
		if err := constraintsPermit(intCert, dns, ips); err != nil {
			return CertResult{}, err
		}
	} else {
		// ONLY the CA keys are guarded, and that asymmetry is the design.
		// Replacing the leaf breaks nothing that has been handed out — every
		// developer's ca.pem still verifies it — so both modes replace it
		// freely. Replacing a CA key invalidates every ca.pem already
		// distributed, which is not an overwrite, it is a fleet-wide outage.
		if err := refuseClobber(req.Force, p(CAKeyFileName), p(IntKeyFileName)); err != nil {
			return CertResult{}, err
		}
		caCert, intCert, intKey, err = createIssuers(req, dns, ips)
		if err != nil {
			return CertResult{}, err
		}
	}

	leafDER, leafKey, err := createLeaf(req, intCert, intKey, dns, ips)
	if err != nil {
		return CertResult{}, err
	}

	// cert.pem is the leaf FOLLOWED BY the intermediate, which is what a PEM
	// bundle conventionally is and what LoadServerTLS already reads: it takes
	// intermediates from Certificate[1:]. The root is deliberately NOT in the
	// bundle — a server that ships its own trust anchor invites a client to
	// trust it, which is the opposite of the point.
	blocks := []*pem.Block{{Type: "CERTIFICATE", Bytes: leafDER}, {Type: "CERTIFICATE", Bytes: intCert.Raw}}
	if err := writePEM(p(CertFileName), 0o644, blocks...); err != nil {
		return CertResult{}, err
	}
	if err := writeKey(p(KeyFileName), leafKey); err != nil {
		return CertResult{}, err
	}
	if !req.LeafOnly {
		if err := writePEM(p(CAFileName), 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw}); err != nil {
			return CertResult{}, err
		}
		if err := writePEM(p(IntFileName), 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: intCert.Raw}); err != nil {
			return CertResult{}, err
		}
	}

	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return CertResult{}, fmt.Errorf("%w: re-reading the leaf we just wrote: %w", ErrCertMaterial, err)
	}
	return CertResult{
		Dir: req.Dir, CAFile: p(CAFileName), CertFile: p(CertFileName), KeyFile: p(KeyFileName),
		DNSNames: dns, IPAddresses: ipStrings(ips),
		LeafNotAfter: leaf.NotAfter, CANotAfter: caCert.NotAfter, ReusedCA: reused,
	}, nil
}

// splitHostNames separates what clients dial into DNS names and IP addresses,
// and adds the loopback set.
//
// Loopback is added unconditionally because the operator's own psql on the
// host is the first thing anyone tries, and a certificate that covers the
// public name but not 127.0.0.1 makes that first attempt fail in a way that
// reads as "TLS is broken" rather than "that name is not on the certificate".
func splitHostNames(hosts []string) (dns []string, ips []net.IP, err error) {
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		// A bracketed IPv6 literal is what an address looks like in a bind
		// string; accept it here rather than mint a certificate for a name
		// that is really an address wearing brackets.
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if ip := net.ParseIP(h); ip != nil {
			if !seenIP[ip.String()] {
				seenIP[ip.String()] = true
				ips = append(ips, ip)
			}
			return
		}
		h = normalizeDNSName(h)
		if h == "" {
			return
		}
		if !seenDNS[h] {
			seenDNS[h] = true
			dns = append(dns, h)
		}
	}
	for _, h := range hosts {
		add(h)
	}
	if len(dns) == 0 && len(ips) == 0 {
		return nil, nil, fmt.Errorf("%w: no host names given; name every address clients will "+
			"dial (frontdoor.tls_host_names), because sslmode=verify-full verifies the NAME and a "+
			"certificate that omits one fails at that client and nowhere else", ErrCertMaterial)
	}
	for _, l := range []string{"localhost", "127.0.0.1", "::1"} {
		add(l)
	}
	sort.Strings(dns)
	return dns, ips, nil
}

// normalizeDNSName puts a name into the one form a certificate may carry.
//
// DNS names are CASE-INSENSITIVE, and a fully-qualified name written with its
// ROOT LABEL — "autodb.example.com." — is the same name as the one without.
// Neither belongs in a certificate verbatim, and the trailing form is not
// merely untidy: Go's x509 REFUSES TO PARSE a dNSName name constraint that
// carries it, so an operator who typed the fully-qualified form got
//
//	refusing to generate this TLS material: parsing the issuing CA we just
//	signed: x509: failed to parse dnsName constraint "autodb.example.com."
//
// which reads as a bug in autodb rather than as "drop the trailing dot". A
// reviewer found the same normalisation missing one layer down, in the
// --leaf-only constraint check, where it only produced a misleading message;
// following it to the source is where the hard failure was.
func normalizeDNSName(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

// permittedRanges is the name-constraint IP set: a single-host range per
// address. Anything wider would permit the CA to vouch for hosts nobody
// intended, which is the whole property being bought here.
func permittedRanges(ips []net.IP) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ips))
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)})
			continue
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
	}
	return out
}

func newKey() (*ecdsa.PrivateKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("%w: generating a key: %w", ErrCertMaterial, err)
	}
	return k, nil
}

func serial() (*big.Int, error) {
	// 128 random bits, per the CA/Browser Forum's rule and for the same
	// reason: a predictable serial is a hash-collision lever.
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("%w: generating a serial: %w", ErrCertMaterial, err)
	}
	return n, nil
}

// createIssuers mints the root and the name-constrained intermediate.
func createIssuers(req CertRequest, dns []string, ips []net.IP) (*x509.Certificate, *x509.Certificate, *ecdsa.PrivateKey, error) {
	caKey, err := newKey()
	if err != nil {
		return nil, nil, nil, err
	}
	caSerial, err := serial()
	if err != nil {
		return nil, nil, nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: caSerial,
		Subject:      pkix.Name{Organization: []string{"autodb"}, CommonName: "autodb front door root CA"},
		NotBefore:    req.Now.Add(-time.Hour),
		NotAfter:     req.Now.Add(req.CALifetime),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// One intermediate below this root and no more.
		MaxPathLen:            1,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: signing the root: %w", ErrCertMaterial, err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: parsing the root we just signed: %w", ErrCertMaterial, err)
	}

	intKey, err := newKey()
	if err != nil {
		return nil, nil, nil, err
	}
	intSerial, err := serial()
	if err != nil {
		return nil, nil, nil, err
	}
	intTmpl := &x509.Certificate{
		SerialNumber: intSerial,
		Subject: pkix.Name{
			Organization: []string{"autodb"},
			CommonName:   "autodb front door issuing CA",
		},
		NotBefore: req.Now.Add(-time.Hour),
		NotAfter:  req.Now.Add(req.CALifetime),
		IsCA:      true,
		KeyUsage:  x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// No CA below this one: it issues leaves and nothing else.
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		BasicConstraintsValid: true,
		// THE CONSTRAINT, and the reason this is strictly safer than mkcert.
		// mkcert installs an UNCONSTRAINED CA into the machine trust store,
		// one that can mint a valid certificate for any name on the internet.
		// This one is never installed machine-wide AND cannot sign outside
		// these names even if its key is stolen.
		//
		// It sits on the INTERMEDIATE rather than the root because Java does
		// not apply a trust anchor's own constraints — measured, not assumed;
		// see the package comment.
		PermittedDNSDomains:         dns,
		PermittedIPRanges:           permittedRanges(ips),
		PermittedDNSDomainsCritical: true,
	}
	intDER, err := x509.CreateCertificate(rand.Reader, intTmpl, caCert, &intKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: signing the issuing CA: %w", ErrCertMaterial, err)
	}
	intCert, err := x509.ParseCertificate(intDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: parsing the issuing CA we just signed: %w", ErrCertMaterial, err)
	}

	if err := writeKey(filepath.Join(req.Dir, CAKeyFileName), caKey); err != nil {
		return nil, nil, nil, err
	}
	if err := writeKey(filepath.Join(req.Dir, IntKeyFileName), intKey); err != nil {
		return nil, nil, nil, err
	}
	return caCert, intCert, intKey, nil
}

// createLeaf mints the server certificate.
func createLeaf(req CertRequest, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey, dns []string, ips []net.IP) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		// NO CommonName. Java checks a leaf's CN as if it were a DNS name
		// whenever a dNSName constraint is present, so a CN outside the
		// permitted set is rejected by pgjdbc while psql and Go accept the
		// same certificate — TLS that works in psql and fails in DataGrip.
		// Measured against its own control; see the package comment. An
		// Organization keeps the subject readable without reintroducing it.
		Subject:     pkix.Name{Organization: []string{"autodb front door"}},
		NotBefore:   req.Now.Add(-time.Hour),
		NotAfter:    req.Now.Add(req.LeafLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    dns,
		IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: signing the server certificate: %w", ErrCertMaterial, err)
	}
	return der, key, nil
}

// loadIssuer reads back the CA and intermediate for --leaf-only.
func loadIssuer(dir string) (*x509.Certificate, *x509.Certificate, *ecdsa.PrivateKey, error) {
	ca, err := readCert(filepath.Join(dir, CAFileName))
	if err != nil {
		return nil, nil, nil, err
	}
	ic, err := readCert(filepath.Join(dir, IntFileName))
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, IntKeyFileName))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: reading %s (--leaf-only needs the issuing CA that "+
			"signed the previous certificate; without it a new leaf cannot chain to the ca.pem "+
			"your developers already hold): %w", ErrCertMaterial, IntKeyFileName, err)
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, nil, nil, fmt.Errorf("%w: %s contains no PEM block", ErrCertMaterial, IntKeyFileName)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: parsing %s: %w", ErrCertMaterial, IntKeyFileName, err)
	}
	return ca, ic, key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrCertMaterial, filepath.Base(path), err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%w: %s contains no PEM block", ErrCertMaterial, filepath.Base(path))
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %w", ErrCertMaterial, filepath.Base(path), err)
	}
	return c, nil
}

// constraintsPermit reports whether the issuing CA may vouch for these names.
//
// This is the --leaf-only guard, and it is a refusal rather than a warning for
// the same reason LoadServerTLS refuses at startup: signing anyway produces a
// certificate that is perfectly well-formed and that every verifying client
// rejects, one connection at a time, with an error each of them reads as their
// own problem.
func constraintsPermit(issuer *x509.Certificate, dns []string, ips []net.IP) error {
	var bad []string
	if len(issuer.PermittedDNSDomains) > 0 {
		for _, h := range dns {
			if !dnsPermitted(h, issuer.PermittedDNSDomains) {
				bad = append(bad, h)
			}
		}
	}
	if len(issuer.PermittedIPRanges) > 0 {
		for _, ip := range ips {
			if !ipPermitted(ip, issuer.PermittedIPRanges) {
				bad = append(bad, ip.String())
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the existing issuing CA is name-constrained and cannot vouch for %s. "+
		"It permits %v and %v. Reissuing the whole chain (drop --leaf-only) mints a CA that covers "+
		"the new names, but it produces a NEW ca.pem that every developer must be given — which is "+
		"exactly the redistribution --leaf-only exists to avoid, so this is a decision rather than "+
		"a retry", ErrCertMaterial, strings.Join(bad, ", "),
		issuer.PermittedDNSDomains, ipNetStrings(issuer.PermittedIPRanges))
}

func ipNetStrings(ns []*net.IPNet) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.String())
	}
	return out
}

// dnsPermitted implements RFC 5280 dNSName subtree matching: a constraint
// matches the name itself and any name below it.
func dnsPermitted(name string, permitted []string) bool {
	// Normalised on BOTH sides. splitHostNames already normalises what this
	// process generates, but `permitted` is read back from a CERTIFICATE — one
	// a previous version, or another tool, may have written — so a name is
	// compared against whatever that file happens to hold.
	//
	// Both divergences a reviewer found here refused a name the CA could
	// vouch for, so the direction was safe. The cost was the MESSAGE: the
	// operator is told the CA is constrained against a name it covers, and
	// "retype it in lower case" is not discoverable from that text.
	name = normalizeDNSName(name)
	for _, p := range permitted {
		p = normalizeDNSName(strings.TrimPrefix(p, "."))
		// The comparison is against "."+p, NOT a bare suffix. A bare
		// strings.HasSuffix would admit "evilexample.com" for a constraint of
		// "example.com" — the classic form of this bug, and the one that would
		// have mattered.
		if p != "" && (name == p || strings.HasSuffix(name, "."+p)) {
			return true
		}
	}
	return false
}

func ipPermitted(ip net.IP, ranges []*net.IPNet) bool {
	for _, r := range ranges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// refuseClobber stops a second run silently replacing key material.
//
// The failure it prevents is not a lost file, it is a SPLIT TRUST STATE: a
// regenerated CA leaves every developer holding a ca.pem that no longer
// verifies anything, and the only symptom is every client failing at once with
// a message about an unknown authority.
func refuseClobber(force bool, paths ...string) error {
	if force {
		return nil
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%w: %s already exists. Overwriting it invalidates every ca.pem "+
				"already distributed, and every client would fail at once with an unknown-authority "+
				"error. Use --leaf-only to reissue the server certificate from the SAME CA (no "+
				"redistribution), or --force if you really mean to replace the CA", ErrCertMaterial, p)
		}
	}
	return nil
}

func writePEM(path string, mode os.FileMode, blocks ...*pem.Block) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("%w: writing %s: %w", ErrCertMaterial, path, err)
	}
	defer f.Close()
	for _, b := range blocks {
		if err := pem.Encode(f, b); err != nil {
			return fmt.Errorf("%w: encoding %s: %w", ErrCertMaterial, path, err)
		}
	}
	// Re-applied because O_CREATE's mode is masked by the process umask, and a
	// certificate nobody can read is a different bug from one everybody can.
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("%w: chmod %s: %w", ErrCertMaterial, path, err)
	}
	return nil
}

// writeKey writes private key material 0600, and the mode is not advisory:
// the key is the whole identity of the front door.
func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("%w: encoding a private key: %w", ErrCertMaterial, err)
	}
	return writePEM(path, 0o600, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}
