package exec

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	drvmysql "github.com/go-sql-driver/mysql"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/golib/dao/mysql"
)

// tlsFiles is a CA, a server certificate it signed for 127.0.0.1, and a client certificate and key
// it signed, written as PEM files in dir.
type tlsFiles struct {
	dir, ca, cert, key string
	server             tls.Certificate
	caCert             *x509.Certificate
	caKey              *ecdsa.PrivateKey
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newCA makes a CA; signed is a certificate it signed for cn (an IP SAN when ip is set).
func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key, der
}

func signed(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, ip net.IP, usage x509.ExtKeyUsage) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	if ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der, keyDER
}

func newTLSFiles(t *testing.T) *tlsFiles {
	t.Helper()
	dir := t.TempDir()
	f := &tlsFiles{dir: dir, ca: filepath.Join(dir, "ca.pem"), cert: filepath.Join(dir, "client.pem"), key: filepath.Join(dir, "client.key")}
	caCert, caKey, caDER := newCA(t, "autodb test CA")
	f.caCert, f.caKey = caCert, caKey
	writePEM(t, f.ca, "CERTIFICATE", caDER)
	f.server, _, _ = signed(t, caCert, caKey, "mysql", net.ParseIP("127.0.0.1"), x509.ExtKeyUsageServerAuth)
	_, certDER, keyDER := signed(t, caCert, caKey, "autodb-client", nil, x509.ExtKeyUsageClientAuth)
	writePEM(t, f.cert, "CERTIFICATE", certDER)
	writePEM(t, f.key, "EC PRIVATE KEY", keyDER)
	return f
}

// fakeMySQL is a MySQL server as far as a TLS login and a ping: it greets with CLIENT_SSL, takes the
// client's SSLRequest, does the TLS handshake (asking for a client certificate), answers the login
// and each COM_PING with OK, and writes down every COM_QUERY.
type fakeMySQL struct {
	ln      net.Listener
	conf    *tls.Config
	mu      sync.Mutex
	clients []string // each TLS client certificate's common name ("" for none)
	queries []string
}

func startFakeMySQL(t *testing.T, server tls.Certificate, clientCAs *x509.CertPool) *fakeMySQL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMySQL{ln: ln, conf: &tls.Config{Certificates: []tls.Certificate{server}, ClientCAs: clientCAs,
		ClientAuth: tls.VerifyClientCertIfGiven}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	return m
}

func (m *fakeMySQL) addr() string { return m.ln.Addr().String() }

func writePacket(w io.Writer, seq byte, payload []byte) error {
	h := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	_, err := w.Write(append(h, payload...))
	return err
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return h[3], p, err
}

var okPacket = []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}

func (m *fakeMySQL) serve(c net.Conn) {
	defer c.Close()
	const caps = 0x00000001 | 0x00000200 | 0x00000800 | 0x00002000 | 0x00008000 | 0x00080000 // long password, 4.1, SSL, transactions, secure connection, plugin auth
	var g bytes.Buffer
	g.WriteByte(10)
	g.WriteString("8.0.36-fake\x00")
	_ = binary.Write(&g, binary.LittleEndian, uint32(7))
	g.WriteString("abcdefgh\x00")
	_ = binary.Write(&g, binary.LittleEndian, uint16(caps&0xffff))
	g.WriteByte(45)
	_ = binary.Write(&g, binary.LittleEndian, uint16(2))
	_ = binary.Write(&g, binary.LittleEndian, uint16(caps>>16))
	g.WriteByte(21)
	g.Write(make([]byte, 10))
	g.WriteString("ijklmnopqrst\x00")
	g.WriteString("mysql_native_password\x00")
	if writePacket(c, 0, g.Bytes()) != nil {
		return
	}
	if _, _, err := readPacket(c); err != nil { // the SSLRequest
		return
	}
	tc := tls.Server(c, m.conf)
	if err := tc.Handshake(); err != nil {
		return
	}
	cn := ""
	if peers := tc.ConnectionState().PeerCertificates; len(peers) > 0 {
		cn = peers[0].Subject.CommonName
	}
	m.mu.Lock()
	m.clients = append(m.clients, cn)
	m.mu.Unlock()
	seq, _, err := readPacket(tc) // the login
	if err != nil || writePacket(tc, seq+1, okPacket) != nil {
		return
	}
	for {
		seq, p, err := readPacket(tc)
		if err != nil || len(p) == 0 {
			return
		}
		switch p[0] {
		case 0x03: // COM_QUERY
			m.mu.Lock()
			m.queries = append(m.queries, string(p[1:]))
			m.mu.Unlock()
		case 0x01: // COM_QUIT
			return
		}
		if writePacket(tc, seq+1, okPacket) != nil {
			return
		}
	}
}

func (m *fakeMySQL) seen() (clients, queries []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.clients...), append([]string(nil), m.queries...)
}

func mysqlDSN(addr string, params ...string) string {
	dsn := "u:p@tcp(" + addr + ")/app"
	if len(params) > 0 {
		dsn += "?" + strings.Join(params, "&")
	}
	return dsn
}

// TestMySQLTLSFromFiles (ADR 1792301900 §4.11, result 12): a DSN naming tls-ca, tls-cert and tls-key
// connects over TLS to a server whose certificate the CA signed, presenting the client certificate;
// the three are never sent to MySQL as variables. A CA that did not sign the server's certificate,
// or a host it was not issued for, is refused; a renewed CA file is read at the next connection.
func TestMySQLTLSFromFiles(t *testing.T) {
	f := newTLSFiles(t)
	roots := x509.NewCertPool()
	roots.AddCert(f.caCert)
	srv := startFakeMySQL(t, f.server, roots)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsn := mysqlDSN(srv.addr(), "tls-ca="+f.ca, "tls-cert="+f.cert, "tls-key="+f.key)
	if err := ValidateDSN(engine.MySQL, dsn); err != nil {
		t.Fatalf("ValidateDSN: %v", err)
	}
	open, err := mysqlTLS(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := drvmysql.ParseDSN(open)
	for _, p := range mysqlTLSParams {
		if _, ok := cfg.Params[p]; ok {
			t.Fatalf("%s left in the DSN: %s", p, open)
		}
	}
	if !strings.HasPrefix(cfg.TLSConfig, "autodb-") {
		t.Fatalf("tls=%q", cfg.TLSConfig)
	}
	conn, err := mysql.OpenNamed(ctx, "tls", open)
	if err != nil {
		t.Fatalf("open over TLS: %v", err)
	}
	conn.Close()
	clients, queries := srv.seen()
	if len(clients) == 0 || clients[0] != "autodb-client" {
		t.Fatalf("the server saw client certificates %q", clients)
	}
	for _, q := range queries {
		if strings.Contains(q, "tls-") {
			t.Fatalf("a TLS file reached MySQL: %q", q)
		}
	}

	// another CA in tls-ca: the server's certificate is refused; the same file renewed with the
	// right CA connects again, read at the connection
	otherCA, _, otherDER := newCA(t, "another CA")
	_ = otherCA
	writePEM(t, f.ca, "CERTIFICATE", otherDER)
	if c, err := mysql.OpenNamed(ctx, "tls", open); err == nil {
		c.Close()
		t.Fatal("a server certificate another CA signed was accepted")
	}
	writePEM(t, f.ca, "CERTIFICATE", f.caCert.Raw)
	if c, err := mysql.OpenNamed(ctx, "tls", open); err != nil {
		t.Fatalf("the renewed CA file was not read again: %v", err)
	} else {
		c.Close()
	}

	// a host the certificate was not issued for
	_, port, _ := net.SplitHostPort(srv.addr())
	wrongHost, err := mysqlTLS(mysqlDSN("localhost:"+port, "tls-ca="+f.ca))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := mysql.OpenNamed(ctx, "tls", wrongHost); err == nil {
		c.Close()
		t.Fatal("a certificate for 127.0.0.1 was accepted for localhost")
	}
}

// TestMySQLTLSParametersAreChecked: the files are absolute paths, a certificate and its key go
// together, the DSN's own tls parameter does not mix with them, and postgres refuses them; a DSN
// with none is opened as it was.
func TestMySQLTLSParametersAreChecked(t *testing.T) {
	for name, c := range map[string]struct {
		eng  engine.Name
		dsn  string
		want string
	}{
		"relative":     {engine.MySQL, mysqlDSN("db:3306", "tls-ca=ca.pem"), "absolute path"},
		"cert alone":   {engine.MySQL, mysqlDSN("db:3306", "tls-cert=/c.pem"), "go together"},
		"key alone":    {engine.MySQL, mysqlDSN("db:3306", "tls-key=/k.pem"), "go together"},
		"with tls":     {engine.MySQL, mysqlDSN("db:3306", "tls=true", "tls-ca=/ca.pem"), "drop its tls parameter"},
		"postgres too": {engine.Postgres, "postgres://u:p@db:5432/app?tls-ca=/ca.pem", "it is MySQL's"},
	} {
		err := ValidateDSN(c.eng, c.dsn)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	plain := mysqlDSN("db:3306", "parseTime=true")
	if got, err := mysqlTLS(plain); err != nil || got != plain {
		t.Fatalf("a DSN with no TLS files: %q, %v", got, err)
	}
	if err := ValidateDSN(engine.MySQL, mysqlDSN("db:3306", fmt.Sprintf("tls-ca=%s", "/etc/ssl/ca.pem"))); err != nil {
		t.Fatalf("tls-ca alone: %v", err)
	}
}
