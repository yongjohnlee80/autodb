package frontdoor_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/frontdoor"
)

// Generated material must satisfy the SAME check the daemon runs at startup.
//
// That is the point of generating it at all, and it is the assertion most
// worth making: LoadServerTLS is the consumer, so a cell that only inspected
// the certificate's fields would be checking my arithmetic rather than the
// thing an operator experiences.

func genAt(t *testing.T, dir string, hosts []string, opts ...func(*frontdoor.CertRequest)) frontdoor.CertResult {
	t.Helper()
	req := frontdoor.CertRequest{Dir: dir, HostNames: hosts, Now: time.Unix(1_800_000_000, 0)}
	for _, o := range opts {
		o(&req)
	}
	res, err := frontdoor.CreateCert(req)
	if err != nil {
		t.Fatalf("CreateCert: %v", err)
	}
	return res
}

func loadFor(t *testing.T, res frontdoor.CertResult, hosts []string, now time.Time) error {
	t.Helper()
	_, err := frontdoor.LoadServerTLS(config.FrontDoor{
		TLSCertFile: res.CertFile, TLSKeyFile: res.KeyFile,
		TLSRootCAFile: res.CAFile, TLSHostNames: hosts,
	}, now)
	return err
}

func TestCreateCert_StartupAcceptsWhatWeGenerated(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	cases := [][]string{
		{"autodb.example.com"},
		{"10.4.1.7"},                       // the AlloyDB case: an address, no DNS name
		{"autodb.example.com", "10.4.1.7"}, // both, which is what a real deployment has
		{"[2001:db8::1]"},                  // a bracketed literal, as a bind string writes it
	}
	for _, hosts := range cases {
		t.Run(strings.Join(hosts, ","), func(t *testing.T) {
			res := genAt(t, t.TempDir(), hosts)
			// Every configured name, and the loopback set the generator adds
			// so the operator's own psql works without a second certificate.
			for _, h := range append(append([]string{}, hosts...), "localhost", "127.0.0.1", "::1") {
				h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
				if err := loadFor(t, res, []string{h}, now); err != nil {
					t.Errorf("the daemon would refuse to start for %q: %v", h, err)
				}
			}
			// THE DECOY. A certificate covering everything would pass every
			// assertion above; sslmode=verify-full exists to reject exactly
			// this, so the generator must not quietly produce a wildcard.
			if err := loadFor(t, res, []string{"not-ours.example.com"}, now); err == nil {
				t.Error("the certificate verifies for a name nobody configured")
			}
			if err := loadFor(t, res, []string{"198.51.100.9"}, now); err == nil {
				t.Error("the certificate verifies for an address nobody configured")
			}
		})
	}
}

// The chain is root -> constrained intermediate -> leaf, and the SHAPE is
// load-bearing rather than decorative.
//
// MEASURED, not assumed: Java does not apply a trust anchor's own name
// constraints, so a constrained self-signed root is enforced by psql and Go and
// IGNORED by pgjdbc. On an intermediate, which is IN the path, all three
// enforce it. A future simplification to "just self-sign the CA" would pass
// every other cell in this file, so this one exists to stop it.
func TestCreateCert_ConstraintLivesOnAnIntermediate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := genAt(t, dir, []string{"autodb.example.com", "10.4.1.7"})

	chain := readChain(t, res.CertFile)
	if len(chain) != 2 {
		t.Fatalf("cert.pem holds %d certificates, want 2 (leaf then intermediate) — a client "+
			"cannot build a path without the intermediate, and the server is the last to know", len(chain))
	}
	leaf, inter := chain[0], chain[1]
	if leaf.IsCA {
		t.Error("the first certificate in cert.pem is a CA; the leaf must come first")
	}
	if !inter.IsCA {
		t.Fatal("the second certificate in cert.pem is not a CA")
	}
	if len(inter.PermittedDNSDomains) == 0 && len(inter.PermittedIPRanges) == 0 {
		t.Fatal("the intermediate carries NO name constraints — this CA can vouch for any name, " +
			"which is the mkcert property this design exists to avoid")
	}
	// And the ROOT must not be the constrained one, because that is the form
	// Java ignores.
	root := readChain(t, res.CAFile)[0]
	if !root.IsCA {
		t.Fatal("ca.pem is not a CA")
	}
	if len(root.PermittedDNSDomains) > 0 || len(root.PermittedIPRanges) > 0 {
		t.Error("the constraints are on the ROOT. Java does not apply a trust anchor's own name " +
			"constraints, so pgjdbc would ignore them while psql enforced them — measured")
	}
	// The root must NOT be shipped in cert.pem: a server that serves its own
	// trust anchor invites a client to trust it.
	for _, c := range chain {
		if c.Equal(root) {
			t.Error("cert.pem contains the root certificate")
		}
	}
}

// The leaf carries NO CommonName, and that is not stylistic.
//
// MEASURED: Java checks a leaf's CN as if it were a DNS name whenever a dNSName
// constraint is present. A leaf with CN=anything-not-under-the-constraint is
// rejected by pgjdbc and accepted by psql and Go — TLS that works in psql and
// fails in DataGrip, with an error pointing nowhere useful. Adding a friendly
// CN back would look like an improvement and would break exactly one client.
func TestCreateCert_LeafHasNoCommonName(t *testing.T) {
	t.Parallel()
	res := genAt(t, t.TempDir(), []string{"autodb.example.com"})
	leaf := readChain(t, res.CertFile)[0]
	if leaf.Subject.CommonName != "" {
		t.Errorf("the leaf carries CommonName %q. Java checks a leaf's CN as a DNS name against "+
			"the CA's dNSName constraints, so this is rejected by pgjdbc (DataGrip) while psql "+
			"and Go accept it", leaf.Subject.CommonName)
	}
	// Still identifiable to a human reading `openssl x509 -text`.
	if len(leaf.Subject.Organization) == 0 {
		t.Error("the leaf has no Organization either, so its subject is entirely blank")
	}
	// SAN-only certificates require a critical SAN extension when the subject
	// is empty; Go sets it, and a client that required it would reject
	// otherwise. Asserted because it is a property of the file, not of Go.
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		t.Error("the leaf carries no SANs at all")
	}
}

// Key material is 0600 and the public half is not.
func TestCreateCert_FileModes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := genAt(t, dir, []string{"autodb.example.com"})
	for _, p := range []string{res.KeyFile,
		filepath.Join(dir, frontdoor.CAKeyFileName), filepath.Join(dir, frontdoor.IntKeyFileName)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("%s is mode %o, want 600 — the key is the whole identity of the front door",
				filepath.Base(p), m)
		}
	}
	// ca.pem is PUBLIC by design and must be readable, or the operator cannot
	// hand it out without a chmod they will not think to do.
	fi, err := os.Stat(res.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o044 == 0 {
		t.Errorf("ca.pem is mode %o; it is the file every developer receives", fi.Mode().Perm())
	}
}

// A second run must not silently replace the CA.
//
// The failure is not a lost file, it is a SPLIT TRUST STATE: every distributed
// ca.pem stops verifying at once, and the only symptom is every client failing
// with an unknown-authority error.
func TestCreateCert_RefusesToReplaceTheCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	genAt(t, dir, []string{"autodb.example.com"})

	_, err := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: []string{"autodb.example.com"}, Now: time.Unix(1_800_000_000, 0),
	})
	if !errors.Is(err, frontdoor.ErrCertMaterial) {
		t.Fatalf("a second run replaced the CA without being asked: %v", err)
	}
	if !strings.Contains(err.Error(), "--leaf-only") {
		t.Errorf("the refusal does not name the operation the operator probably wants: %v", err)
	}
	// --force is the escape hatch, and it must work or the refusal is a wall.
	if _, ferr := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: []string{"autodb.example.com"},
		Now: time.Unix(1_800_000_000, 0), Force: true,
	}); ferr != nil {
		t.Errorf("--force could not replace the CA: %v", ferr)
	}
}

// --leaf-only reissues from the SAME CA. That is the whole point: rotation
// without redistribution.
func TestCreateCert_LeafOnlyKeepsTheCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hosts := []string{"autodb.example.com"}
	first := genAt(t, dir, hosts)
	firstCA := readChain(t, first.CAFile)[0]
	firstLeaf := readChain(t, first.CertFile)[0]

	second := genAt(t, dir, hosts, func(r *frontdoor.CertRequest) {
		r.LeafOnly = true
		r.Now = time.Unix(1_800_000_000, 0).Add(24 * time.Hour)
	})
	if !second.ReusedCA {
		t.Error("--leaf-only reported creating a new CA")
	}
	secondCA := readChain(t, second.CAFile)[0]
	if !firstCA.Equal(secondCA) {
		t.Fatal("--leaf-only changed ca.pem; every developer's copy would stop verifying, which " +
			"is exactly what this mode exists to avoid")
	}
	secondLeaf := readChain(t, second.CertFile)[0]
	if firstLeaf.Equal(secondLeaf) {
		t.Fatal("--leaf-only did not actually issue a new certificate")
	}
	// And the reissued leaf still starts the daemon.
	if err := loadFor(t, second, hosts, time.Unix(1_800_000_000, 0).Add(24*time.Hour)); err != nil {
		t.Errorf("the daemon would refuse the reissued certificate: %v", err)
	}
}

// --leaf-only for an address the CA cannot vouch for is REFUSED rather than
// signed.
//
// x509.CreateCertificate signs it happily; the failure would surface at every
// client as an unexplained verification error, read as a client-side problem.
// This is the same argument LoadServerTLS makes for checking at startup.
func TestCreateCert_LeafOnlyRefusesNamesOutsideTheConstraint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	genAt(t, dir, []string{"autodb.example.com"})

	_, err := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: []string{"elsewhere.example.net"},
		Now: time.Unix(1_800_000_000, 0), LeafOnly: true,
	})
	if !errors.Is(err, frontdoor.ErrCertMaterial) {
		t.Fatalf("--leaf-only signed a name the CA is constrained against: %v", err)
	}
	if !strings.Contains(err.Error(), "elsewhere.example.net") {
		t.Errorf("the refusal does not name what it refused: %v", err)
	}

	// CONTROL: a name INSIDE the constraint still works, so the guard is not
	// simply refusing everything.
	if _, ok := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: []string{"autodb.example.com"},
		Now: time.Unix(1_800_000_000, 0), LeafOnly: true,
	}); ok != nil {
		t.Fatalf("--leaf-only refused a permitted name too: %v", ok)
	}
	// And a subdomain, which RFC 5280 dNSName subtrees permit.
	if _, ok := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: dir, HostNames: []string{"replica.autodb.example.com"},
		Now: time.Unix(1_800_000_000, 0), LeafOnly: true,
	}); ok != nil {
		t.Errorf("--leaf-only refused a subdomain of a permitted name: %v", ok)
	}
}

// No host names at all is a refusal with a reason, not a certificate for
// localhost that looks like it worked.
func TestCreateCert_RefusesWithNoHostNames(t *testing.T) {
	t.Parallel()
	_, err := frontdoor.CreateCert(frontdoor.CertRequest{
		Dir: t.TempDir(), Now: time.Unix(1_800_000_000, 0),
	})
	if !errors.Is(err, frontdoor.ErrCertMaterial) {
		t.Fatalf("generated a certificate for nothing in particular: %v", err)
	}
}

// The generated constraint must actually bite. Go's verifier enforces name
// constraints, so this asserts the property end to end rather than trusting
// that the fields were set.
func TestCreateCert_ConstraintRejectsAForeignLeaf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := genAt(t, dir, []string{"autodb.example.com"})

	// A leaf for a name outside the constraint, signed by the real issuing CA
	// — the certificate an attacker holding ca.key would try to mint.
	foreign := mintForeignLeaf(t, dir, "elsewhere.example.net")
	roots := x509.NewCertPool()
	caPEM, err := os.ReadFile(res.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(caPEM)
	inter := x509.NewCertPool()
	inter.AddCert(readChain(t, res.CertFile)[1])

	if _, verr := foreign.Verify(x509.VerifyOptions{
		DNSName: "elsewhere.example.net", Roots: roots, Intermediates: inter,
		CurrentTime: time.Unix(1_800_000_000, 0),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); verr == nil {
		t.Fatal("the name-constrained CA vouched for a name outside its constraint; the " +
			"constraint is decorative")
	}
}

func readChain(t *testing.T, path string) []*x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []*x509.Certificate
	for {
		blk, rest := pem.Decode(b)
		if blk == nil {
			break
		}
		c, perr := x509.ParseCertificate(blk.Bytes)
		if perr != nil {
			t.Fatalf("parsing %s: %v", filepath.Base(path), perr)
		}
		out = append(out, c)
		b = rest
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no certificates", filepath.Base(path))
	}
	return out
}

// mintForeignLeaf is the attacker's move: somebody holding the issuing CA's
// key signs a certificate for a name that CA was never meant to cover.
//
// Built here with crypto/x509 directly rather than through a test-only export
// on the production package. CreateCert REFUSES this by design, and a hook that
// let a test bypass that refusal would be a hook that lets anything else bypass
// it too.
func mintForeignLeaf(t *testing.T, dir, name string) *x509.Certificate {
	t.Helper()
	issuer := readChain(t, filepath.Join(dir, frontdoor.IntFileName))[0]
	keyPEM, err := os.ReadFile(filepath.Join(dir, frontdoor.IntKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		t.Fatal("the issuing key holds no PEM block")
	}
	issuerKey, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{Organization: []string{"attacker"}},
		NotBefore:    time.Unix(1_800_000_000, 0).Add(-time.Hour),
		NotAfter:     time.Unix(1_800_000_000, 0).Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &k.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("signing the foreign leaf: %v", err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A fully-qualified name, written with its root label, must WORK.
//
// It used to fail at CA creation with "parsing the issuing CA we just signed:
// x509: failed to parse dnsName constraint" — Go's x509 refuses a dNSName name
// constraint carrying a trailing dot. The message reads as an autodb bug rather
// than as "drop the trailing dot", and an operator who types the form their DNS
// zone uses is not making a mistake.
//
// A reviewer found the same missing normalisation one layer down, in the
// --leaf-only constraint check, where it only produced a misleading message.
// This cell pins the harder failure at the source.
func TestCreateCert_AcceptsAFullyQualifiedName(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	res := genAt(t, t.TempDir(), []string{"autodb.example.com.", "DB.Example.COM"})

	// Both are normalised into the one form a certificate may carry, and the
	// uppercase one is not a second, distinct SAN.
	if got := strings.Join(res.DNSNames, ","); got != "autodb.example.com,db.example.com,localhost" {
		t.Fatalf("DNS SANs = %q; a trailing root label and mixed case must normalise, and must "+
			"not produce duplicate entries", got)
	}
	// And the daemon starts for the names an operator would actually type,
	// in either form.
	for _, dial := range []string{"autodb.example.com", "db.example.com", "DB.EXAMPLE.COM"} {
		if err := loadFor(t, res, []string{dial}, now); err != nil {
			t.Errorf("the daemon would refuse to start for %q: %v", dial, err)
		}
	}
}

// dnsPermitted's own table, because --leaf-only's refusal message is built on
// it and a wrong answer there sends an operator to change the wrong thing.
//
// The row that matters most is evilexample.com: a bare strings.HasSuffix would
// admit it for a constraint of example.com, which is the classic form of this
// bug and the only PERMISSIVE way to get it wrong.
func TestCreateCert_LeafOnlyNameMatching(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	genAt(t, dir, []string{"example.com"})

	cases := []struct {
		name    string
		allowed bool
		why     string
	}{
		{"example.com", true, "the domain itself"},
		{"db.example.com", true, "a subdomain"},
		{"a.b.example.com", true, "a deeper subdomain"},
		{"DB.Example.COM", true, "DNS names are case-insensitive"},
		{"db.example.com.", true, "fully qualified, with the root label"},
		{"evilexample.com", false, "NOT a subdomain — the classic suffix bug"},
		{"example.com.evil.net", false, "the constraint appears, but not as a suffix"},
		{"elsewhere.example.net", false, "a different domain entirely"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := frontdoor.CreateCert(frontdoor.CertRequest{
				Dir: dir, HostNames: []string{tc.name},
				Now: time.Unix(1_800_000_000, 0), LeafOnly: true,
			})
			refused := errors.Is(err, frontdoor.ErrCertMaterial)
			if refused == tc.allowed {
				t.Fatalf("%s (%s): refused=%v, want refused=%v (err %v)",
					tc.name, tc.why, refused, !tc.allowed, err)
			}
		})
	}
}
