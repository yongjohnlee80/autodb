package frontdoor

// A CHAIN FAILURE MUST NOT NAME THE CERTIFICATE'S HOST NAMES.
//
// `leaf.Verify` subsumes THREE different failures — the SAN check, the chain,
// and server-auth key usage — and every one of them was formatted the same
// way, around the leaf's DNSNames. That is the right thing to print for the
// first and actively misleading for the other two.
//
// What it cost: the droplet crash-looped 60 times with
//
//	refusing to serve with this TLS material: the certificate does not verify
//	for "165.232.128.152" (it carries names [localhost]) — … A missing
//	intermediate looks exactly like this: x509: certificate signed by unknown
//	authority
//
// The real cause was tls_root_ca_file UNSET, so the private CA was not in the
// trust roots. The message named the SANs, which invited checking the SANs, and
// then offered "a missing intermediate looks exactly like this" — a fourth
// possibility. One line, three explanations, pointing at the least likely.
//
// Not one clause of it was false. That is what made it expensive: nothing
// looked wrong with it.
//
// The branch is chosen from the verification error's TYPE via errors.As, never
// by matching its text.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 1. A NAME MISMATCH NAMES THE SANs, and blames nothing else.
//
// This is the one case where the SANs are the answer, so it is also the
// control that the taxonomy did not simply stop printing them.
func TestTLSTaxonomy_NameMismatchNamesTheSANs(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ch := issueChain(t, []string{"localhost"}, now.Add(-time.Hour), now.Add(time.Hour))

	// Configured for an address the leaf does not carry — the droplet's shape.
	_, err := LoadServerTLS(fdWith(ch.bundle, ch.key, ch.ca, "165.232.128.152"), now)
	if err == nil {
		t.Fatal("a certificate that does not cover the configured name was accepted")
	}
	msg := err.Error()

	if !strings.Contains(msg, "localhost") {
		t.Errorf("a NAME mismatch does not name the SANs, which is the one thing it "+
			"should: %s", msg)
	}
	if !strings.Contains(msg, "165.232.128.152") {
		t.Errorf("the message does not name the address that was asked for: %s", msg)
	}
	// AND IT DOES NOT BLAME THE TRUST ROOT. The chain is fine here; sending an
	// operator to tls_root_ca_file would be the same defect pointed the other
	// way.
	if strings.Contains(msg, "tls_root_ca_file") {
		t.Errorf("a name mismatch blames the trust root: %s", msg)
	}
	if strings.Contains(msg, "missing intermediate") {
		t.Errorf("a name mismatch offers a chain explanation: %s", msg)
	}
}

// 2. AN UNKNOWN AUTHORITY NAMES THE TRUST SOURCE, and does not lead with the
// SANs. This cell reproduces the droplet.
func TestTLSTaxonomy_UnknownAuthorityNamesTheTrustSource(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ch := issueChain(t, []string{"localhost"}, now.Add(-time.Hour), now.Add(time.Hour))

	// The leaf COVERS the configured name; what is missing is the private CA
	// in the trust roots — tls_root_ca_file unset, exactly as on the droplet.
	fd := fdWith(ch.bundle, ch.key, "", "localhost")
	_, err := LoadServerTLS(fd, now)
	if err == nil {
		t.Skip("this host's system roots accepted a private CA, which cannot happen on " +
			"a normal machine and would make this cell meaningless")
	}
	msg := err.Error()

	// THE TRUST SOURCE ACTUALLY USED, which is the one thing the operator can
	// act on.
	if !strings.Contains(msg, "system roots") {
		t.Errorf("the failure does not say which trust store was consulted: %s", msg)
	}
	if !strings.Contains(msg, "tls_root_ca_file") {
		t.Errorf("the failure does not name the setting to check: %s", msg)
	}
	// NOT LEADING WITH THE SANs. They may appear later — they are still
	// useful — but the first line is about the trust source.
	first := strings.SplitN(msg, "\n", 2)[0]
	if strings.Contains(first, "localhost") {
		t.Errorf("the first line of a chain failure names the certificate's host "+
			"names, which is what sent an operator to check the SANs: %s", first)
	}
	// AND THE UNSET ROOT IS CONTEXT, NOT A DIAGNOSIS. It does not prove an
	// intermediate is missing, and it does not prove one is present.
	if strings.Contains(msg, "missing intermediate looks exactly like this") {
		t.Errorf("the message still asserts a cause it cannot know: %s", msg)
	}
}

// 3. A CERTIFICATE WITHOUT SERVER-AUTH KEY USAGE SAYS SO.
//
// A certificate issued for a different job is neither a name problem nor a
// chain problem, and the old message described it as both.
func TestTLSTaxonomy_WrongKeyUsageSaysSo(t *testing.T) {
	t.Parallel()
	now := time.Now()
	certPath, keyPath, caPath := issueClientAuthOnly(t, "localhost", now)

	_, err := LoadServerTLS(fdWith(certPath, keyPath, caPath, "localhost"), now)
	if err == nil {
		t.Fatal("a certificate with no server-auth key usage was accepted")
	}
	msg := err.Error()

	if !strings.Contains(msg, "server authentication") {
		t.Errorf("the failure does not say the certificate is not for server "+
			"authentication: %s", msg)
	}
	// It is not a name problem and not a trust problem.
	if strings.Contains(msg, "tls_root_ca_file") {
		t.Errorf("a key-usage failure blames the trust root: %s", msg)
	}
}

// 4. THE FALLBACK IS LABELLED.
//
// Required, and it must not be silently equivalent to one of the branches: a Go
// release that changed these error shapes would otherwise route every failure
// into whichever branch happened to match, and an operator would read a
// confident wrong answer. The label is what tells the next reader that the
// classification did not fire.
func TestTLSTaxonomy_AnUnclassifiedFailureIsLabelled(t *testing.T) {
	t.Parallel()

	got := describeVerifyFailure("example.com", []string{"other.example"}, "", errors.New("x509: some future failure mode"))
	if !strings.Contains(got, "unclassified") {
		t.Errorf("an error matching none of the known shapes is not labelled as "+
			"unclassified, so a future Go would produce a confident wrong branch: %s", got)
	}
	// It still carries everything a reader needs, since nothing narrowed it.
	for _, want := range []string{"example.com", "other.example", "some future failure mode"} {
		if !strings.Contains(got, want) {
			t.Errorf("the unclassified message omits %q: %s", want, got)
		}
	}

	// POSITIVE CONTROL: a KNOWN shape is not labelled unclassified, or the
	// assertion above would pass for a message that says it about everything.
	known := describeVerifyFailure("example.com", []string{"other.example"}, "",
		x509.UnknownAuthorityError{})
	if strings.Contains(known, "unclassified") {
		t.Errorf("a recognised failure is reported as unclassified: %s", known)
	}
}

// 5. THE TRUST SOURCE IS THE CONFIGURED PATH WHEN ONE IS SET.
//
// Without this, cell 2 passes on a message that says "system roots" for every
// install — the same defect as before, wearing a new sentence.
func TestTLSTaxonomy_TheTrustSourceIsTheConfiguredPath(t *testing.T) {
	t.Parallel()

	got := describeVerifyFailure("example.com", []string{"example.com"},
		"/etc/autodb/tls/ca.pem", x509.UnknownAuthorityError{})
	if !strings.Contains(got, "/etc/autodb/tls/ca.pem") {
		t.Errorf("a configured trust root is not named in the failure: %s", got)
	}
	if strings.Contains(got, "system roots") {
		t.Errorf("an install WITH a private CA is told it used the system roots: %s", got)
	}
}

// issueClientAuthOnly writes a chain whose leaf carries ExtKeyUsageClientAuth
// only — a certificate for a different job.
func issueClientAuthOnly(t *testing.T, host string, now time.Time) (certPath, keyPath, caPath string) {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "usage-test CA"},
		NotBefore:             now.Add(-2 * time.Hour),
		NotAfter:              now.Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// CLIENT auth only: the whole point of the fixture.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	write := func(name string, blocks ...*pem.Block) string {
		p := filepath.Join(dir, name)
		var buf []byte
		for _, b := range blocks {
			buf = append(buf, pem.EncodeToMemory(b)...)
		}
		if err := os.WriteFile(p, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kb, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return write("cert.pem", &pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		write("key.pem", &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
		write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// AN IP IDENTITY IS AN IDENTITY, AND THE MESSAGE MUST SAY SO.
//
// Found on review, with a probe I reproduced before folding: a certificate
// issued for 192.0.2.10 with the front door configured for 192.0.2.11 produced
// a headline reading `carries names [localhost]` while the x509 error appended
// to the same string said the certificate was valid for 192.0.2.10, 127.0.0.1
// and ::1. The message contradicted its own evidence and blamed a DNS name that
// had nothing to do with the failure.
//
// The cause was the CALL SITE, not the formatter: describeVerifyFailure was
// handed leaf.DNSNames and presented it as the complete carried set.
// --create-cert has taken IP SANs since DNS-or-IP issuance landed, and they go
// in IPAddresses, so for an IP-only certificate the set printed was empty.
//
// Driven through LoadServerTLS rather than by calling the formatter directly.
// That is deliberate: the formatter was always capable of printing whatever it
// was given, so a cell that called it with a hand-built list would have passed
// against the defect.
func TestVerifyFailure_IPIdentitiesAppearInBothBranches(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// A certificate carrying BOTH kinds, which is the ordinary shape
	// --create-cert produces for a host reachable by name and by address.
	// Both must survive into the message: printing only one is the defect in
	// its other direction.
	const certIP = "192.0.2.10"
	const certDNS = "autodb.example.com"
	const wrongIP = "192.0.2.11"

	t.Run("hostname mismatch names the address the certificate carries", func(t *testing.T) {
		t.Parallel()
		c := issueChain(t, []string{certDNS, certIP}, now.Add(-time.Hour), now.Add(time.Hour))

		// Configured for an address the certificate does NOT carry.
		_, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, wrongIP), now)
		if err == nil {
			t.Fatal("a certificate that does not cover the configured address was accepted")
		}
		msg := err.Error()

		if !strings.Contains(msg, "does not cover the configured name") {
			t.Fatalf("not classified as a name mismatch, so this cell is measuring "+
				"something else:\n%s", msg)
		}
		// THE FINDING, asserted against AUTODB'S OWN DISPLAY rather than
		// against the message as a whole.
		//
		// This distinction is not pedantry: a mutation proved it. x509's
		// HostnameError text ends with "valid for 192.0.2.10, ..." and is
		// appended to this very string, so `strings.Contains(msg, certIP)`
		// passed against the defect -- it was matching the verifier's words,
		// not autodb's. The exact rendered list is the only assertion that
		// cannot be satisfied by the borrowed evidence.
		want := "carries [" + certDNS + " " + certIP + "]"
		if !strings.Contains(msg, want) {
			t.Errorf("autodb's own identity display does not read %q, so the headline "+
				"still does not name what the certificate carries — whatever the "+
				"appended x509 error happens to mention:\n%s", want, msg)
		}
	})

	t.Run("unknown authority names them too", func(t *testing.T) {
		t.Parallel()
		c := issueChain(t, []string{certDNS, certIP}, now.Add(-time.Hour), now.Add(time.Hour))

		// No tls_root_ca_file: the private chain is checked against the system
		// roots and cannot verify. This is the branch where the verifier does
		// NOT append the SANs itself, so autodb's own display is the only place
		// the identities appear at all.
		fd := fdWith(c.bundle, c.key, "", certDNS)
		_, err := LoadServerTLS(fd, now)
		if err == nil {
			t.Fatal("a private chain verified against the system roots")
		}
		msg := err.Error()

		if !strings.Contains(msg, "does not verify against") {
			t.Fatalf("not classified as an authority failure:\n%s", msg)
		}
		// Same exact-list assertion. Here x509 appends nothing to borrow from,
		// which is precisely why this branch is the more serious of the two:
		// autodb's display is the only place an identity can appear at all.
		want := "is for [" + certDNS + " " + certIP + "]"
		if !strings.Contains(msg, want) {
			t.Errorf("autodb's own identity display does not read %q. x509 does not "+
				"append SANs for an unknown-authority failure, so this display is the "+
				"ONLY place an IP identity can appear:\n%s", want, msg)
		}
	})
}

// AND certIdentities ITSELF, on the shapes that produced the wrong answer.
//
// The unit half: the cells above drive the real path, and this pins the
// function's contract on the two degenerate shapes — an IP-only certificate,
// which reported an empty set, and a bare one, which must stay empty rather
// than inventing an identity.
func TestCertIdentities_CoversBothSANKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		leaf *x509.Certificate
		want []string
	}{
		{
			name: "IP only — the shape that printed an empty set",
			leaf: &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("192.0.2.10")}},
			want: []string{"192.0.2.10"},
		},
		{
			name: "both kinds, DNS first",
			leaf: &x509.Certificate{
				DNSNames:    []string{"autodb.example.com"},
				IPAddresses: []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("::1")},
			},
			want: []string{"autodb.example.com", "192.0.2.10", "::1"},
		},
		{
			// No SANs at all stays empty. A message saying a certificate
			// carries nothing is TRUE here, and that is the honest output.
			name: "no SANs",
			leaf: &x509.Certificate{},
			want: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := certIdentities(tc.leaf)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("index %d = %q, want %q (got %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
