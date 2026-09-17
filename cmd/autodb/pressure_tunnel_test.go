package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/autodb/webserver"
)

// THE RECIPE IS RUN, NOT READ.
//
// FOUND IN REVIEW, and the gap was exactly the one the operations page is
// about. The doc cells hold the page's NUMBERS to the build — the port matches,
// the bind is loopback, the page says admin-only. None of them has ever moved a
// byte through a forward. So the page's actual claim, "paste this and you are
// looking at the view", rested on three string comparisons.
//
// That is the same defect this whole milestone keeps turning up: a cell
// asserting a described output cannot see whether the described mechanism does
// any work. Here the mechanism is an SSH port-forward to a loopback-only bind,
// and the thing that would have shipped broken is the one thing nobody can
// check by reading: whether the right-hand address is resolved at the far end.
//
// WHAT IS REAL HERE: the ssh binary, invoked with the flags taken out of the
// page itself; a genuine SSH transport with publickey auth and a direct-tcpip
// channel; the listen address the production gateway computes; and an HTTP
// round-trip that has to survive all of it. WHAT IS A FIXTURE: the far-side
// server is a bare handler rather than the whole gateway, because what is under
// test is reaching the address, and standing up a browser surface needs a live
// daemon behind it. The SSH server is in-process because the sanctioned image
// ships an ssh client and no sshd, and installing one would put the proof in a
// container nobody else runs.
func TestPressureTunnel_TheDocumentedForwardReachesTheSurface(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		// NOT A SKIP. A skip here is how the containment probe went green under
		// a shell wrapper: the cell reported success for a run that never
		// happened. If the image loses its ssh client the recipe cannot be
		// proven, and that is a failure to say out loud.
		t.Fatalf("no ssh client on PATH, so the page's one command cannot be run: %v", err)
	}

	docLocal, docRemote := docForwardPorts(t)
	if docRemote != defaultWebPort {
		t.Fatalf("the page forwards to remote port %d and the build defaults to %d",
			docRemote, defaultWebPort)
	}

	// The far end, at the address the PRODUCTION code computes. A port or bind
	// change moves this and the forward stops arriving.
	const body = "pressure-view-reached-through-the-forward"
	farAddr := webserver.ListenAddr(defaultWebPort)
	farLn, err := net.Listen("tcp", farAddr)
	if err != nil {
		t.Fatalf("cannot bind the browser surface at %s: %v — the recipe names this "+
			"address, so a cell that cannot occupy it cannot prove anything about it",
			farAddr, err)
	}
	defer farLn.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = srv.Serve(farLn) }()
	defer func() { _ = srv.Close() }()

	tun := startTunnelServer(t)

	// The left-hand port is the one the page says to change when it collides
	// with something local, and here it always does: the far end is on this
	// same host, holding docLocal already.
	localPort := freePort(t)
	if localPort == docLocal {
		t.Fatalf("the picked local port %d collided with the page's", localPort)
	}

	key := writeClientKey(t, tun.clientKey)
	cmd := exec.Command(sshBin,
		"-N",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", localPort, docRemote),
		"-p", strconv.Itoa(tun.port),
		"-i", key,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"tester@127.0.0.1",
	)
	var errBuf syncBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("ssh: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// ssh -N never says it is ready; the forward being connectable is the signal.
	local := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(20 * time.Second)
	var up bool
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", local, 200*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			up = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		t.Fatalf("the forward never came up on %s. ssh said: %s", local, errBuf.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + local + "/")
	if err != nil {
		t.Fatalf("nothing answered through the forward: %v (ssh: %s)", err, errBuf.String())
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("the forward reached something, and it was not the browser surface: %q",
			string(got))
	}

	// THE RIGHT-HAND ADDRESS IS RESOLVED AT THE FAR END, which is the sentence
	// on the page that nobody can verify by reading, and the reason a
	// loopback-only bind is reachable at all. The SSH server records what the
	// client actually asked it to dial.
	host, port := tun.lastForward()
	if port != defaultWebPort {
		t.Errorf("the client asked the far end for port %d, want %d", port, defaultWebPort)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("the client asked the far end for %q; the page says the right-hand "+
			"127.0.0.1 is resolved on the remote host, and a non-loopback address "+
			"there would be reaching something else entirely", host)
	}
}

// AND THE SURFACE IS NOT REACHABLE WITHOUT THE FORWARD.
//
// The other half, and the one that makes the first half mean something: a
// forward that works proves reachability, not confinement. If the bind widened
// to 0.0.0.0 the cell above would still pass — the tunnel would carry the
// request just as happily — while the page's central claim, and the security
// posture it describes, had quietly inverted.
func TestPressureTunnel_TheSurfaceIsUnreachableOffLoopback(t *testing.T) {
	farAddr := webserver.ListenAddr(defaultWebPort)
	ln, err := net.Listen("tcp", farAddr)
	if err != nil {
		t.Fatalf("cannot bind %s: %v", farAddr, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	routable := routableAddr(t)
	c, err := net.DialTimeout("tcp", net.JoinHostPort(routable, strconv.Itoa(defaultWebPort)),
		2*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatalf("the browser surface answered on %s, a routable address. It reports "+
			"other people's session counts and the source addresses being refused, and "+
			"the operations page tells an operator to tunnel to it precisely because it "+
			"is supposed to be unreachable this way", routable)
	}
}

// --- fixtures -------------------------------------------------------------

// docForwardPorts takes the forward straight out of the page, so the cell runs
// what an operator would paste rather than what this file believes they would.
func docForwardPorts(t *testing.T) (local, remote int) {
	t.Helper()
	doc := readPressureDoc(t)
	m := regexp.MustCompile(`ssh -N -L (\d+):127\.0\.0\.1:(\d+) `).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("the page no longer carries a pasteable ssh forward to run")
	}
	local, _ = strconv.Atoi(m[1])
	remote, _ = strconv.Atoi(m[2])
	return local, remote
}

// tunnelServer is a disposable, headless SSH server that does exactly one
// thing: honour direct-tcpip, which is what `ssh -L` is made of.
type tunnelServer struct {
	port      int
	clientKey ed25519.PrivateKey

	mu       sync.Mutex
	destHost string
	destPort int
}

func (s *tunnelServer) lastForward() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destHost, s.destPort
}

func (s *tunnelServer) note(host string, port int) {
	s.mu.Lock()
	s.destHost, s.destPort = host, port
	s.mu.Unlock()
}

func startTunnelServer(t *testing.T) *tunnelServer {
	t.Helper()

	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) != string(authorized.Marshal()) {
				return nil, fmt.Errorf("unknown key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &tunnelServer{port: ln.Addr().(*net.TCPAddr).Port, clientKey: clientKey}
	go func() {
		for {
			nc, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go s.serve(nc, cfg)
		}
	}()
	return s
}

func (s *tunnelServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for nch := range chans {
		if nch.ChannelType() != "direct-tcpip" {
			// `ssh -N` opens nothing else; anything else is a cell that has
			// drifted from what it claims to exercise.
			_ = nch.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		var req struct {
			DestAddr string
			DestPort uint32
			SrcAddr  string
			SrcPort  uint32
		}
		if err := ssh.Unmarshal(nch.ExtraData(), &req); err != nil {
			_ = nch.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
			continue
		}
		s.note(req.DestAddr, int(req.DestPort))

		// Resolved HERE — on the "remote host" — which is the property the page
		// claims and this fixture exists to make observable.
		up, derr := net.DialTimeout("tcp",
			net.JoinHostPort(req.DestAddr, strconv.Itoa(int(req.DestPort))), 5*time.Second)
		if derr != nil {
			_ = nch.Reject(ssh.ConnectionFailed, derr.Error())
			continue
		}
		ch, chReqs, aerr := nch.Accept()
		if aerr != nil {
			_ = up.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() { _, _ = io.Copy(ch, up); _ = ch.CloseWrite() }()
		go func() { _, _ = io.Copy(up, ch); _ = up.Close() }()
	}
}

func writeClientKey(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	blk, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	// ssh refuses a key anyone else can read, which is the one permission bug
	// that would make this cell fail for a reason unrelated to its claim.
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// routableAddr is an address on this host that is NOT loopback — what somebody
// on the same network would reach the machine by.
func routableAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil {
			continue
		}
		return n.IP.String()
	}
	t.Fatal("this host has no non-loopback IPv4 address, so confinement cannot be " +
		"observed here — the claim is about what a second machine can reach")
	return ""
}

// syncBuf collects a subprocess's stderr from its own goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.b = append(s.b, p...)
	s.mu.Unlock()
	return len(p), nil
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}
