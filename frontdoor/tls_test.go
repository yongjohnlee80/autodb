package frontdoor

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
)

// issue writes a self-signed certificate and key, and returns their paths.
// Real material rather than fixtures: every rejection below is about what
// x509 actually decides, and a hand-written fixture would be asserting my
// model of that instead.
func issue(t *testing.T, hosts []string, notBefore, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "autodb-test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              hosts,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certPath)
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = certOut.Close()
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, _ := os.Create(keyPath)
	_ = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	_ = keyOut.Close()
	return certPath, keyPath
}

func fdWith(certPath, keyPath string, hosts ...string) config.FrontDoor {
	return config.FrontDoor{
		Enabled: true, Bind: "127.0.0.1:5432",
		TLSCertFile: certPath, TLSKeyFile: keyPath, TLSHostNames: hosts,
		ReservedHeadroom: 4,
	}
}

// The listener never binds with an identity it cannot prove (matrix row
// 2.1b). Every case here is a real deployment mistake, and each must be
// reported as ITSELF — "TLS error" sends an operator to inspect the wrong
// thing.
func TestLoadServerTLS_RefusesUnusableMaterial(t *testing.T) {
	t.Parallel()
	now := time.Now()
	good, goodKey := issue(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))

	// Positive control FIRST: good material loads. Without it, every refusal
	// below could be a function that refuses everything.
	cfg, err := LoadServerTLS(fdWith(good, goodKey, "autodb.example.com"), now)
	if err != nil {
		t.Fatalf("valid material was refused (%v); this test cannot observe a real refusal either", err)
	}
	if cfg.MinVersion != 0x0303 { // TLS 1.2
		t.Errorf("MinVersion = %#x, want TLS 1.2 — everything below it is broken in public", cfg.MinVersion)
	}
	if len(cfg.NextProtos) != 0 {
		t.Errorf("NextProtos = %v; v1 refuses PostgreSQL 17 direct TLS (matrix row 2.1a), and "+
			"advertising ALPN is part of not offering it", cfg.NextProtos)
	}

	expired, expiredKey := issue(t, []string{"autodb.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	future, futureKey := issue(t, []string{"autodb.example.com"}, now.Add(time.Hour), now.Add(48*time.Hour))
	other, otherKey := issue(t, []string{"autodb.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))

	for _, tc := range []struct {
		name string
		fd   config.FrontDoor
		says string
	}{
		{"a missing certificate file",
			fdWith(filepath.Join(t.TempDir(), "absent.pem"), goodKey), "tls_cert_file"},
		{"a missing key file",
			fdWith(good, filepath.Join(t.TempDir(), "absent.pem")), "tls_key_file"},
		{"an expired certificate",
			fdWith(expired, expiredKey), "expired"},
		{"a certificate that is not valid yet",
			fdWith(future, futureKey), "not valid until"},
		{"a certificate beside the wrong key",
			fdWith(good, otherKey), "do not form a usable pair"},
		{"a certificate that does not cover the configured name",
			fdWith(other, otherKey, "other.example.com"), "does not cover"},
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
	junkDir := t.TempDir()
	junk := filepath.Join(junkDir, "junk.pem")
	if err := os.WriteFile(junk, []byte("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServerTLS(fdWith(junk, goodKey), now); !errors.Is(err, ErrTLSMaterial) {
		t.Errorf("unparsable certificate material = %v, want ErrTLSMaterial", err)
	}
}
