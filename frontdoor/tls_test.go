package frontdoor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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
)

// chain issues a THREE-level chain — root CA, intermediate, leaf — and
// writes the files a real deployment has: the leaf alone (`cert.pem`), the
// leaf plus its intermediate (`fullchain.pem`), and the root.
//
// Three levels and not two, deliberately. A leaf signed directly by the
// configured root needs no intermediate, so a two-level fixture cannot
// express the defect this exists to catch: my first version of this helper
// was two-level and the missing-intermediate cell passed against it for the
// wrong reason. The real misconfiguration is a renewal that wrote cert.pem
// where fullchain.pem was meant, and that only bites when there IS an
// intermediate to omit.
type chain struct{ leafOnly, bundle, ca, key string }

func issueChain(t *testing.T, hosts []string, notBefore, notAfter time.Time) chain {
	t.Helper()
	dir := t.TempDir()

	mkCA := func(cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(time.Now().UnixNano() + int64(len(cn))),
			Subject:               pkix.Name{CommonName: cn},
			NotBefore:             notBefore.Add(-2 * time.Hour),
			NotAfter:              notAfter.Add(2 * time.Hour),
			KeyUsage:              x509.KeyUsageCertSign,
			IsCA:                  true,
			BasicConstraintsValid: true,
		}
		signer, signerKey := tmpl, key
		if parent != nil {
			signer, signerKey = parent, parentKey
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, der
	}

	rootCert, rootKey, rootDER := mkCA("autodb-test-root", nil, nil)
	interCert, interKey, interDER := mkCA("autodb-test-intermediate", rootCert, rootKey)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 7),
		Subject:      pkix.Name{CommonName: "autodb-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, interCert, &leafKey.PublicKey, interKey)
	if err != nil {
		t.Fatal(err)
	}

	write := func(name string, blocks ...*pem.Block) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			_ = pem.Encode(f, b)
		}
		_ = f.Close()
		return path
	}
	cert := func(der []byte) *pem.Block { return &pem.Block{Type: "CERTIFICATE", Bytes: der} }
	kb, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = interDER
	return chain{
		leafOnly: write("cert.pem", cert(leafDER)),
		bundle:   write("fullchain.pem", cert(leafDER), cert(interDER)),
		ca:       write("ca.pem", cert(rootDER)),
		key:      write("key.pem", &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	}
}
func fdWith(certPath, keyPath, caPath string, hosts ...string) config.FrontDoor {
	return config.FrontDoor{
		Enabled: true, Bind: "127.0.0.1:5432",
		TLSCertFile: certPath, TLSKeyFile: keyPath, TLSRootCAFile: caPath,
		TLSHostNames: hosts, ReservedHeadroom: 4,
	}
}

func TestLoadServerTLS_RefusesUnusableMaterial(t *testing.T) {
	t.Parallel()
	now := time.Now()
	const host = "autodb.example.com"
	good := issueChain(t, []string{host}, now.Add(-time.Hour), now.Add(24*time.Hour))

	// Positive control FIRST: a properly chained certificate loads. Without
	// it, every refusal below could be a function that refuses everything.
	cfg, err := LoadServerTLS(fdWith(good.bundle, good.key, good.ca, host), now)
	if err != nil {
		t.Fatalf("valid material was refused (%v); this test cannot observe a real refusal either", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 — everything below it is broken in public", cfg.MinVersion)
	}
	if len(cfg.NextProtos) != 0 {
		t.Errorf("NextProtos = %v; v1 refuses PostgreSQL 17 direct TLS (matrix row 2.1a), and "+
			"advertising ALPN is part of not offering it", cfg.NextProtos)
	}

	expired := issueChain(t, []string{host}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	future := issueChain(t, []string{host}, now.Add(time.Hour), now.Add(48*time.Hour))
	other := issueChain(t, []string{host}, now.Add(-time.Hour), now.Add(24*time.Hour))

	for _, tc := range []struct {
		name string
		fd   config.FrontDoor
		says string
	}{
		{"a missing certificate file",
			fdWith(filepath.Join(t.TempDir(), "absent.pem"), good.key, good.ca, host), "tls_cert_file"},
		{"a missing key file",
			fdWith(good.bundle, filepath.Join(t.TempDir(), "absent.pem"), good.ca, host), "tls_key_file"},
		{"an expired certificate",
			fdWith(expired.bundle, expired.key, expired.ca, host), "expired"},
		{"a certificate that is not valid yet",
			fdWith(future.bundle, future.key, future.ca, host), "not valid until"},
		{"a certificate beside the wrong key",
			fdWith(good.bundle, other.key, good.ca, host), "do not form a usable pair"},
		{"a certificate that does not cover the configured name",
			fdWith(good.bundle, good.key, good.ca, "other.example.com"), "does not verify for"},

		// MF2. The one the first version of this file missed entirely: a
		// leaf served WITHOUT its intermediate parses perfectly, is in date,
		// and carries the right name — it simply cannot be built into a path
		// to any root. Every verifying client rejects it and the listener is
		// the last to know. This is the ordinary shape of a real
		// misconfiguration: a renewal that wrote cert.pem where fullchain.pem
		// was meant.
		{"a leaf served without its intermediate",
			fdWith(good.leafOnly, good.key, good.ca, host), "does not verify for"},

		// And the trust root itself must be real. Falling back to system
		// roots on an unreadable CA file would turn a path typo into
		// "verification passed against roots that were never meant to sign
		// this" — a weaker check wearing the same green.
		{"an unreadable trust root",
			fdWith(good.bundle, good.key, filepath.Join(t.TempDir(), "absent.pem"), host),
			"tls_root_ca_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadServerTLS(tc.fd, now)
			if err == nil {
				t.Fatalf("%s was accepted; the listener would bind with an identity it cannot prove",
					tc.name)
			}
			if !errors.Is(err, ErrTLSMaterial) {
				t.Errorf("err = %v, want ErrTLSMaterial", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the message does not say what is wrong (%q missing): %v", tc.says, err)
			}
		})
	}

	// Garbage in the files is refused too, and not by luck: an operator who
	// points these at the wrong path usually points them at SOMETHING.
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServerTLS(fdWith(junk, good.key, good.ca, host), now); !errors.Is(err, ErrTLSMaterial) {
		t.Errorf("unparsable certificate material = %v, want ErrTLSMaterial", err)
	}
}

// The bundle that a real deployment writes — leaf plus its issuer — verifies
// against the configured private CA. This is the ADR's second sanctioned
// case, and without a passing cell for it the chain check above could be
// satisfied by something that only ever accepts publicly-rooted material.
func TestLoadServerTLS_AcceptsAPrivateCAChain(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := issueChain(t, []string{"a.example.com", "b.example.com"},
		now.Add(-time.Hour), now.Add(24*time.Hour))

	if _, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "a.example.com", "b.example.com"), now); err != nil {
		t.Fatalf("a private-CA chain covering both configured names was refused: %v", err)
	}
	// Every configured name is checked, not just the first.
	_, err := LoadServerTLS(fdWith(c.bundle, c.key, c.ca, "a.example.com", "missing.example.com"), now)
	if err == nil {
		t.Fatal("a name the certificate does not carry was accepted because an earlier one passed")
	}
	if !strings.Contains(err.Error(), "missing.example.com") {
		t.Errorf("the message names the wrong host: %v", err)
	}
}
