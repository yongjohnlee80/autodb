// Package remotectl is Remote Control: the switch that runs the daemon's
// remote listener, and what keeps it running.
//
// The switch lives in the meta store (remote.ControlKey), so it survives a
// restart, and an admin changes it at run time (Set). While it is on, a
// supervisor keeps a remote listener serving into the RPC server's fan-in:
// a listener that cannot bind, at the first try or after it failed, is tried
// again with backoff, and Status says so. Turning it off closes the listener
// and ends every live remote connection. The local surface is never touched.
package remotectl

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// The states Status reports.
const (
	// StateOff: Remote Control is off; no remote listener exists.
	StateOff = "off"
	// StateListening: the remote listener is serving.
	StateListening = "listening"
	// StateRetrying: Remote Control is on but the listener is not serving
	// (it could not bind, its host key could not be loaded, or it failed);
	// it is tried again at NextTry.
	StateRetrying = "retrying"
)

// Status is what Remote Control is doing, for the admin's screen.
type Status struct {
	On        bool
	State     string
	Addr      string    // where it listens, when listening
	HostKeyFP string    // the fingerprint clients pin, once loaded
	Err       string    // why it is not listening, when retrying
	NextTry   time.Time // when it tries again, when retrying
	Live      int       // live remote connections
}

// Config is what Remote Control runs with.
type Config struct {
	Cfg     config.Config
	Store   *meta.Store // where the switch is read at Start
	Auth    *auth.Service
	Limiter *auth.RemoteLimiter
	Fan     *remote.FanIn
	Version string
	Logf    func(string)
	// Seams for tests: how the listener binds, and the retry backoff
	// (1s doubling, capped at a minute, when zero).
	Listen               func(network, addr string) (net.Listener, error)
	RetryFirst, RetryMax time.Duration
}

// Control is Remote Control for one daemon.
type Control struct {
	cfg Config
	reg *remote.Registry

	// switchMu serializes Start, Set and Close, so a supervisor being stopped
	// is gone before another starts and the two never contend for the port.
	switchMu sync.Mutex
	mu       sync.Mutex
	ctx      context.Context // the daemon's, from Start
	status   Status
	run      *supervisor // nil while off
}

// New builds Remote Control, off until Start reads the switch.
func New(cfg Config) *Control {
	if cfg.Listen == nil {
		cfg.Listen = net.Listen
	}
	if cfg.RetryFirst <= 0 {
		cfg.RetryFirst = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = time.Minute
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string) {}
	}
	return &Control{cfg: cfg, reg: remote.NewRegistry(), ctx: context.Background(), status: Status{State: StateOff}}
}

// Registry is the live remote connections.
func (c *Control) Registry() *remote.Registry { return c.reg }

// Start reads the switch and, when it is on, starts the listener's
// supervision. ctx is the daemon's: when it ends, so does the supervision.
// It fails only when the switch cannot be read.
func (c *Control) Start(ctx context.Context) error {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	v, _, err := c.cfg.Store.GetMeta(ctx, remote.ControlKey)
	if err != nil {
		return fmt.Errorf("reading the Remote Control switch: %w", err)
	}
	if v == "on" {
		c.turnOn()
	}
	return nil
}

// Set turns Remote Control on or off: the switch and its audit row are
// recorded first (auth.SetRemoteControl), then the listener is started or
// stopped. Turning it off ends every live remote connection, after the switch
// is committed, so none can reconnect against the old state.
func (c *Control) Set(ctx context.Context, on bool, byUserID int64, ip string) error {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	if _, err := c.cfg.Auth.SetRemoteControl(ctx, on, byUserID, ip); err != nil {
		return fmt.Errorf("recording the Remote Control switch: %w", err)
	}
	if on {
		c.turnOn()
	} else {
		c.turnOff()
	}
	return nil
}

// Status is what Remote Control is doing now.
func (c *Control) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Live = c.reg.Len()
	return s
}

// Close is the daemon's shutdown: it stops the listener and its supervision,
// then ends every live remote connection. The switch is left as it is, so the
// next start serves again.
func (c *Control) Close() {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	c.mu.Lock()
	r := c.run
	c.run = nil
	c.mu.Unlock()
	if r != nil {
		r.stop()
	}
	c.reg.Close(remote.All)
}

func (c *Control) turnOn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil {
		return
	}
	c.status = Status{On: true, State: StateRetrying, Err: "starting", HostKeyFP: c.status.HostKeyFP}
	c.run = &supervisor{c: c, ctx: c.ctx, stopped: make(chan struct{})}
	c.run.wg.Add(1)
	go c.run.loop()
}

func (c *Control) turnOff() {
	c.mu.Lock()
	r := c.run
	c.run = nil
	c.status = Status{State: StateOff, HostKeyFP: c.status.HostKeyFP}
	c.mu.Unlock()
	if r != nil {
		r.stop()
	}
	if n := c.reg.Close(remote.All); n > 0 {
		c.cfg.Logf(fmt.Sprintf("remote control turned off: %d remote connection(s) ended", n))
	}
}

// setStatus updates the status for s, while s is the running supervisor: one
// being stopped does not overwrite what turning Remote Control off reported.
func (c *Control) setStatus(s *supervisor, update func(*Status)) {
	c.mu.Lock()
	if c.run == s {
		update(&c.status)
	}
	c.mu.Unlock()
}

// supervisor keeps one remote listener serving while Remote Control is on.
type supervisor struct {
	c       *Control
	ctx     context.Context
	stopped chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func (s *supervisor) stop() {
	s.once.Do(func() { close(s.stopped) })
	s.wg.Wait()
}

// loop binds the listener, serves it until it fails or the supervisor stops,
// and tries again with backoff whenever it is not serving: after a failure,
// and when the first bind fails too.
func (s *supervisor) loop() {
	defer s.wg.Done()
	c := s.c
	wait := c.cfg.RetryFirst
	for {
		l, err := s.bind()
		if err != nil {
			next := time.Now().Add(wait)
			c.setStatus(s, func(st *Status) { st.State, st.Err, st.NextTry, st.Addr = StateRetrying, err.Error(), next, "" })
			c.cfg.Logf(fmt.Sprintf("REMOTE LISTENER NOT SERVING, next try in %v: %v", wait, err))
			select {
			case <-s.stopped:
				return
			case <-s.ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, c.cfg.RetryMax)
			continue
		}
		wait = c.cfg.RetryFirst
		c.cfg.Fan.Add(l)
		c.setStatus(s, func(st *Status) {
			st.State, st.Err, st.NextTry, st.Addr, st.HostKeyFP = StateListening, "", time.Time{}, l.Addr().String(), l.HostKeyFingerprint()
		})
		c.cfg.Logf(fmt.Sprintf("remote listener on %s, host key %s", l.Addr(), l.HostKeyFingerprint()))
		select {
		case <-s.stopped:
			c.cfg.Fan.Remove(l)
			return
		case <-s.ctx.Done():
			c.cfg.Fan.Remove(l)
			return
		case <-l.Done():
		}
		c.cfg.Fan.Remove(l)
		if l.Err() == nil {
			return // closed deliberately
		}
		c.cfg.Logf(fmt.Sprintf("REMOTE LISTENER FAILED, restarting it: %v", l.Err()))
	}
}

// bind loads the host key (creating it the first time) and binds the
// listener. Without its limiter it refuses: a remote surface with no bound on
// guesses does not serve.
func (s *supervisor) bind() (*remote.Listener, error) {
	c := s.c
	if c.cfg.Limiter == nil {
		return nil, fmt.Errorf("remote listener: the remote limiter is not available")
	}
	path, err := c.cfg.Cfg.HostKeyPath()
	if err != nil {
		return nil, fmt.Errorf("host key path: %w", err)
	}
	host, fp, err := remote.LoadOrCreateHostKey(path)
	if err != nil {
		return nil, err
	}
	c.setStatus(s, func(st *Status) { st.HostKeyFP = fp })
	tcp, err := c.cfg.Listen("tcp", c.cfg.Cfg.Remote.BindAddr())
	if err != nil {
		return nil, fmt.Errorf("remote listener: %w", err)
	}
	l, err := remote.Listen(tcp, s.listenerConfig(host, fp))
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return l, nil
}

func (s *supervisor) listenerConfig(host ssh.Signer, fp string) remote.Config {
	c := s.c
	lim := c.cfg.Limiter
	return remote.Config{
		HostKey: host, HostKeyFP: fp,
		Authorize:          c.cfg.Auth.RemoteKeyOwner,
		MaxUnauthenticated: c.cfg.Cfg.Remote.HandshakeSlots(),
		HandshakeTimeout:   c.cfg.Cfg.Remote.HandshakeLimit(),
		Version:            c.cfg.Version,
		Logf:               func(format string, args ...any) { c.cfg.Logf(fmt.Sprintf(format, args...)) },
		Registry:           c.reg,
		Admit: func(ip string) bool {
			ok, _ := lim.Admit(s.ctx, ip)
			return ok
		},
		Denied: func(ip, reason, offeredKeyFP string, userID int64) {
			Deny(lim, c.cfg.Logf, ip, reason, offeredKeyFP, userID)
		},
	}
}

// Deny counts a refused remote connection. A spill failure is said loudly:
// the limiter has paused remote admission, which is the safe state, and an
// operator needs to know why.
func Deny(lim *auth.RemoteLimiter, logf func(string), ip, reason, offeredKeyFP string, userID int64) {
	if err := lim.Deny(ip, auth.DenialReason(reason), offeredKeyFP, userID); err != nil {
		logf(fmt.Sprintf("REMOTE DENIAL COULD NOT BE SPILLED, remote admission paused: %v", err))
	}
}

// NewLimiter builds the daemon's remote limiter from [remote]. It is built at
// every start, Remote Control on or off, so denials a previous start left
// pending are replayed before any remote connection is admitted.
func NewLimiter(cfg config.Config, svc *auth.Service) (*auth.RemoteLimiter, error) {
	spill, err := cfg.DenialSpillPath()
	if err != nil {
		return nil, fmt.Errorf("remote denial spill path: %w", err)
	}
	return svc.NewRemoteLimiter(auth.LimiterConfig{
		BlockAfter: cfg.Remote.BlockAfter(),
		BlockFor:   cfg.Remote.BlockFor(),
		SpillPath:  spill,
	})
}
