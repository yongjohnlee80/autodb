package main

import (
	"context"
	"fmt"
	"net"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// startRemote starts the remote listener into fan when Remote Control is on
// (the store's remote.ControlKey is "on"), and returns the address it bound,
// or nil when it is off, and what stops it.
//
// The listener admits only SSH keys registered on a user's profile, through
// svc.RemoteKeyOwner. What reaches the RPC server through it is the remote
// surface (rpc: a narrower method set, no detail disclosure).
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
	tcp, err := net.Listen("tcp", cfg.Remote.BindAddr())
	if err != nil {
		return nil, noop, fmt.Errorf("remote listener: %w", err)
	}
	l, err := remote.Listen(tcp, remote.Config{
		HostKey: host, HostKeyFP: fp,
		Authorize:          svc.RemoteKeyOwner,
		MaxUnauthenticated: cfg.Remote.HandshakeSlots(),
		HandshakeTimeout:   cfg.Remote.HandshakeLimit(),
		Version:            version,
		Logf:               func(format string, args ...any) { onLog(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		_ = tcp.Close()
		return nil, noop, err
	}
	fan.Add(l)
	onLog(fmt.Sprintf("remote listener on %s, host key %s", l.Addr(), fp))
	return l.Addr(), func() { fan.Remove(l) }, nil
}
