package exec

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	drvmysql "github.com/go-sql-driver/mysql"
)

// MYSQL TLS FROM FILES. A MySQL DSN may name its TLS files by path on the
// server's host: tls-ca (the CA that signed the server's certificate; the system's roots without
// it), tls-cert and tls-key (a client certificate, both or neither). go-sql-driver takes TLS only
// as a config registered by name, so the server registers one per set of files and host, and opens
// the DSN with tls=<that name> in their place. The three are never left in the DSN: the driver
// would send an unknown parameter to MySQL as a session variable.
//
// The files are read when a connection is made, never at registration, so a renewed certificate is
// used from the next connection on.

// The DSN parameters naming the TLS files.
const (
	mysqlTLSCA   = "tls-ca"
	mysqlTLSCert = "tls-cert"
	mysqlTLSKey  = "tls-key"
)

var mysqlTLSParams = []string{mysqlTLSCA, mysqlTLSCert, mysqlTLSKey}

// mysqlTLSFiles is a parsed MySQL DSN's TLS files, checked: absolute paths, a certificate and its
// key together, and no tls parameter of the DSN's own beside them. ok is false when none is named.
func mysqlTLSFiles(cfg *drvmysql.Config) (ca, cert, key string, ok bool, err error) {
	ca, cert, key = cfg.Params[mysqlTLSCA], cfg.Params[mysqlTLSCert], cfg.Params[mysqlTLSKey]
	if ca == "" && cert == "" && key == "" {
		return "", "", "", false, nil
	}
	for _, p := range []struct{ name, path string }{{mysqlTLSCA, ca}, {mysqlTLSCert, cert}, {mysqlTLSKey, key}} {
		if p.path != "" && !filepath.IsAbs(p.path) {
			return "", "", "", false, fmt.Errorf("exec: mysql DSN %s must be an absolute path on the server's host", p.name)
		}
	}
	if (cert == "") != (key == "") {
		return "", "", "", false, errors.New("exec: mysql DSN tls-cert and tls-key go together")
	}
	if cfg.TLSConfig != "" || cfg.TLS != nil {
		return "", "", "", false, errors.New("exec: mysql DSN tls-ca, tls-cert and tls-key set TLS: drop its tls parameter")
	}
	return ca, cert, key, true, nil
}

// mysqlTLS turns a MySQL DSN's TLS files into a TLS config registered with the driver under a name
// of the files' and the host's own, and returns the DSN with the three removed and tls=<name> set. A
// DSN that names none is returned as it is.
func mysqlTLS(dsn string) (string, error) {
	cfg, err := drvmysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("exec: invalid mysql DSN: %w", err)
	}
	ca, cert, key, ok, err := mysqlTLSFiles(cfg)
	if err != nil || !ok {
		return dsn, err
	}
	host := cfg.Addr
	if h, _, err := net.SplitHostPort(cfg.Addr); err == nil {
		host = h
	}
	sum := sha256.Sum256([]byte(ca + "\x00" + cert + "\x00" + key + "\x00" + host))
	name := "autodb-" + hex.EncodeToString(sum[:8])
	if err := drvmysql.RegisterTLSConfig(name, mysqlTLSConfig(ca, cert, key, host)); err != nil {
		return "", fmt.Errorf("exec: mysql TLS: %w", err)
	}
	for _, p := range mysqlTLSParams {
		delete(cfg.Params, p)
	}
	cfg.TLSConfig, cfg.TLS = name, nil
	return cfg.FormatDSN(), nil
}

// mysqlTLSConfig verifies the server against ca (the system's roots when ""), for host, and offers
// cert and key when set; each file is read at each handshake. The standard verification is
// replaced, not skipped: VerifyConnection checks the chain and the name itself, since the roots are
// read per connection.
func mysqlTLSConfig(ca, cert, key, host string) *tls.Config {
	conf := &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection below verifies the chain and the host
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("exec: mysql TLS: the server sent no certificate")
			}
			opts := x509.VerifyOptions{DNSName: host, Intermediates: x509.NewCertPool()}
			if ca != "" {
				pem, err := os.ReadFile(ca)
				if err != nil {
					return fmt.Errorf("exec: mysql TLS: reading tls-ca: %w", err)
				}
				opts.Roots = x509.NewCertPool()
				if !opts.Roots.AppendCertsFromPEM(pem) {
					return errors.New("exec: mysql TLS: tls-ca holds no PEM certificate")
				}
			}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		},
	}
	if cert != "" {
		conf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("exec: mysql TLS: reading tls-cert and tls-key: %w", err)
			}
			return &c, nil
		}
	}
	return conf
}
