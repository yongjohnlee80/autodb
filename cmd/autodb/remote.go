package main

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

// Seams for the remote listener's supervision cells: how it binds, and how
// long it waits before trying again. Production uses net.Listen and a backoff
// from one second, doubling, capped at a minute.
var (
	remoteListen     = net.Listen
	remoteRetryFirst = time.Second
	remoteRetryMax   = time.Minute
)

// startRemote starts the remote listener into fan when Remote Control is on
// (the store's remote.ControlKey is "on"), and returns the address it bound,
// or nil when it is off, and what stops it.
//
// The listener admits only SSH keys registered on a user's profile, through
// svc.RemoteKeyOwner. What reaches the RPC server through it is the remote
// surface (rpc: a narrower method set, no detail disclosure).
//
// Once running it is SUPERVISED: if its TCP listener fails, the failure is
// said loudly and the listener is started again, with backoff, until it binds
// or stop is called. The local surface is never touched by any of it.
func startRemote(ctx context.Context, cfg config.Config, store *meta.Store, svc *auth.Service,
	fan *remote.FanIn, version string, onLog func(string)) (net.Addr, func(), error) {
	noop := func() {}
	v, _, err := store.GetMeta(ctx, remote.ControlKey)
	if err != nil {
		return nil, noop, fmt.Errorf("reading the Remote Control switch: %w", err)
	}
	if v != "on" {
		return nil, noop, nil
	}
	path, err := cfg.HostKeyPath()
	if err != nil {
		return nil, noop, fmt.Errorf("host key path: %w", err)
	}
	host, fp, err := remote.LoadOrCreateHostKey(path)
	if err != nil {
		return nil, noop, err
	}
	sup := &remoteSupervisor{cfg: cfg, svc: svc, fan: fan, version: version, onLog: onLog,
		host: host, fp: fp, stopped: make(chan struct{})}
	l, err := sup.bind()
	if err != nil {
		return nil, noop, err
	}
	fan.Add(l)
	onLog(fmt.Sprintf("remote listener on %s, host key %s", l.Addr(), fp))
	sup.wg.Add(1)
	go sup.watch(ctx, l)
	return l.Addr(), sup.stop, nil
}

// remoteSupervisor keeps the remote listener running while Remote Control is
// on.
type remoteSupervisor struct {
	cfg     config.Config
	svc     *auth.Service
	fan     *remote.FanIn
	version string
	onLog   func(string)
	host    ssh.Signer
	fp      string

	stopped chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func (s *remoteSupervisor) bind() (*remote.Listener, error) {
	tcp, err := remoteListen("tcp", s.cfg.Remote.BindAddr())
	if err != nil {
		return nil, fmt.Errorf("remote listener: %w", err)
	}
	l, err := remote.Listen(tcp, remote.Config{
		HostKey: s.host, HostKeyFP: s.fp,
		Authorize:          s.svc.RemoteKeyOwner,
		MaxUnauthenticated: s.cfg.Remote.HandshakeSlots(),
		HandshakeTimeout:   s.cfg.Remote.HandshakeLimit(),
		Version:            s.version,
		Logf:               func(format string, args ...any) { s.onLog(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return l, nil
}

// watch waits for the running listener to stop. Stopped by stop or the
// daemon's shutdown, it is removed and watch returns; stopped because it
// failed, it is started again with backoff.
func (s *remoteSupervisor) watch(ctx context.Context, l *remote.Listener) {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopped:
			s.fan.Remove(l)
			return
		case <-ctx.Done():
			s.fan.Remove(l)
			return
		case <-l.Done():
		}
		cause := l.Err()
		s.fan.Remove(l)
		if cause == nil {
			return // closed deliberately
		}
		s.onLog(fmt.Sprintf("REMOTE LISTENER FAILED, restarting it: %v", cause))
		next, ok := s.rebind(ctx)
		if !ok {
			return
		}
		s.fan.Add(next)
		s.onLog(fmt.Sprintf("remote listener back on %s", next.Addr()))
		l = next
	}
}

// rebind binds a new listener, waiting between tries (doubling, capped),
// until it succeeds or the supervisor is stopped.
func (s *remoteSupervisor) rebind(ctx context.Context) (*remote.Listener, bool) {
	wait := remoteRetryFirst
	for {
		select {
		case <-s.stopped:
			return nil, false
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		l, err := s.bind()
		if err == nil {
			return l, true
		}
		wait = min(wait*2, remoteRetryMax)
		s.onLog(fmt.Sprintf("REMOTE LISTENER STILL DOWN, next try in %v: %v", wait, err))
	}
}

// stop ends supervision and the listener, and waits for both.
func (s *remoteSupervisor) stop() {
	s.once.Do(func() { close(s.stopped) })
	s.wg.Wait()
}
