package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/golib/logger"

	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
)

// The front door's ENTRY POINT (F0f).
//
// Everything below the listener has its own cells. What none of them can show
// is that a configured front door is actually started — and that is precisely
// the class of defect this slice exists to fix: the lease cap and the resident
// budget were both implemented, both tested, and both set only by tests, so in
// a running daemon they were zero. A guard nobody wires is documentation.

// selfSigned writes a certificate and key covering one name, and returns
// their paths.
func selfSigned(t *testing.T, host string, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if werr := os.WriteFile(certPath, certPEM, 0o600); werr != nil {
		t.Fatal(werr)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); werr != nil {
		t.Fatal(werr)
	}
	return certPath, keyPath
}

func frontDoorConfig(t *testing.T, notAfter time.Time) config.Config {
	t.Helper()
	cert, key := selfSigned(t, "autodb.example.com", notAfter)
	var cfg config.Config
	cfg.Exec.PoolMaxConns = 8
	cfg.FrontDoor = config.FrontDoor{
		Enabled: true, Bind: "127.0.0.1:0",
		TLSCertFile: cert, TLSKeyFile: key,
		TLSHostNames:  []string{"autodb.example.com"},
		TLSRootCAFile: cert, // the self-signed leaf is its own root
	}
	return cfg
}

// A CONFIGURED FRONT DOOR IS ACTUALLY LISTENING, with the engine behind it.
//
// The discriminating assertion is the PROMPT. A listener started without an
// authenticator answers a completed startup with the uniform denial, not with
// AuthenticationCleartextPassword — so receiving the prompt is what proves the
// engine was passed, and receiving a denial would prove the surface was wired
// as an empty shell that refuses everyone.
func TestStartFrontDoor_ListensAndHasTheEngineBehindIt(t *testing.T) {
	t.Parallel()
	cfg := frontDoorConfig(t, time.Now().Add(24*time.Hour))
	eng := coreexec.New(nil, nil)
	t.Cleanup(func() { _ = eng.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _, err := startFrontDoor(ctx, cfg, eng, logger.Nop{})
	if err != nil {
		t.Fatalf("startFrontDoor: %v", err)
	}
	if l == nil {
		t.Fatal("the front door is enabled in the configuration and no listener was started")
	}
	defer l.Close()

	raw, err := net.DialTimeout("tcp", l.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))

	ssl := make([]byte, 8)
	binary.BigEndian.PutUint32(ssl[0:4], 8)
	binary.BigEndian.PutUint32(ssl[4:8], 80877103)
	if _, werr := raw.Write(ssl); werr != nil {
		t.Fatal(werr)
	}
	answer := make([]byte, 1)
	if _, rerr := raw.Read(answer); rerr != nil || answer[0] != 'S' {
		t.Fatalf("the listener did not offer TLS: answer=%q err=%v", answer, rerr)
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the client is not the subject
	if herr := tc.Handshake(); herr != nil {
		t.Fatalf("handshake: %v — the configured material did not reach the listener", herr)
	}

	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 3<<16)
	for _, kv := range []string{"user", "root", "database", "gold"} {
		body = append(body, []byte(kv)...)
		body = append(body, 0)
	}
	body = append(body, 0)
	packet := make([]byte, 4)
	binary.BigEndian.PutUint32(packet, uint32(len(body)+4))
	if _, werr := tc.Write(append(packet, body...)); werr != nil {
		t.Fatal(werr)
	}

	fe := pgproto3.NewFrontend(tc, tc)
	msg, rerr := fe.Receive()
	if rerr != nil {
		t.Fatalf("reading the server's answer: %v", rerr)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		t.Fatalf("the server answered %T; a listener with no engine behind it answers a "+
			"completed startup with the uniform denial, so the prompt is what proves the "+
			"engine was actually passed", msg)
	}
}

// A DISABLED front door starts nothing, and asks for no certificate.
func TestStartFrontDoor_DisabledStartsNothing(t *testing.T) {
	t.Parallel()
	var cfg config.Config
	cfg.FrontDoor = config.FrontDoor{Enabled: false, Bind: "127.0.0.1:0"}
	l, _, err := startFrontDoor(context.Background(), cfg, coreexec.New(nil, nil), logger.Nop{})
	if err != nil {
		t.Fatalf("a disabled front door returned an error: %v", err)
	}
	if l != nil {
		l.Close()
		t.Error("a disabled front door started a listener")
	}
}

// UNUSABLE TLS MATERIAL FAILS THE DAEMON rather than degrading to a daemon
// without a front door.
//
// An operator who configured the surface and got a running process without it
// would have no reason to look — the failure would surface later, as clients
// unable to connect to something the daemon never started.
func TestStartFrontDoor_UnusableMaterialFailsTheStart(t *testing.T) {
	t.Parallel()
	// Expired: valid PEM, valid key pair, and not something that may serve.
	cfg := frontDoorConfig(t, time.Now().Add(-time.Hour))
	l, _, err := startFrontDoor(context.Background(), cfg, coreexec.New(nil, nil), logger.Nop{})
	if err == nil {
		if l != nil {
			l.Close()
		}
		t.Fatal("an expired certificate was accepted; the front door would listen with an " +
			"identity it cannot prove")
	}
	if l != nil {
		l.Close()
		t.Error("a listener was returned alongside the error")
	}
}

// AN UNEXPECTED SERVE FAILURE STOPS THE DAEMON (lector PR #38 r0 must-fix 2).
//
// The first version reduced it to a log line while the RPC surface kept
// running, so a configured front door could vanish behind a daemon that
// looked healthy in every other respect — and the one condition an operator
// most needs to see would be a line in a file nobody greps until something
// else has already gone wrong.
//
// Driven through the supervision seam with a synthetic error, the way
// watchLease's cells drive theirs: the failure this guards against is the
// listener stopping, and how it stopped is not what is under test.
func TestSuperviseFrontDoor_AnUnexpectedFailureStopsServing(t *testing.T) {
	t.Parallel()
	serveErr := make(chan error, 1)
	stopped := make(chan struct{})
	var warned string

	fired := superviseFrontDoor(context.Background(), serveErr,
		func() { close(stopped) }, func(m string) { warned = m })

	serveErr <- errors.New("accept: bad file descriptor")

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the front door failed and the daemon kept serving; a configured surface that " +
			"is gone must not hide behind a healthy-looking process")
	}
	select {
	case err := <-fired:
		if err == nil {
			t.Error("no error was reported for the failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failure was not reported, so the exit would look like a clean shutdown")
	}
	if warned == "" {
		t.Error("nothing was written for the operator")
	}
}

// A CLEAN STOP FIRES NOTHING, or every ordinary shutdown would report itself
// as a failure and the signal above would be worthless.
func TestSuperviseFrontDoor_ACleanStopIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
	}{
		{"a nil error", nil},
		{"the context's own cancellation", context.Canceled},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			serveErr := make(chan error, 1)
			stopped := make(chan struct{})
			fired := superviseFrontDoor(context.Background(), serveErr,
				func() { close(stopped) }, func(string) {})
			serveErr <- c.err

			select {
			case <-stopped:
				t.Fatal("a clean stop stopped the daemon")
			case err := <-fired:
				t.Fatalf("a clean stop was reported as a failure: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// The supervisor lets go when the serve context does, rather than outliving
// the thing it watches.
func TestSuperviseFrontDoor_StopsWithTheServeContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	fired := superviseFrontDoor(ctx, make(chan error), func() { close(stopped) }, func(string) {})
	cancel()
	select {
	case <-stopped:
		t.Error("cancelling the serve context was reported as a front-door failure")
	case err := <-fired:
		t.Errorf("a cancelled context fired: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
