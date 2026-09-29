package auth

import (
	"context"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Surface is where a request arrived: the local surface (the unix socket,
// loopback TCP, the web gateway, an in-process frontend) or the remote one
// (a connection the remote SSH listener accepted).
type Surface int

const (
	// SurfaceLocal is every request that did not arrive over the remote
	// listener.
	SurfaceLocal Surface = iota
	// SurfaceRemote is a request on a connection the remote listener
	// accepted.
	SurfaceRemote
)

// Caller is who is presenting a token, as far as the transport knows: the
// surface it arrived on and, for a remote connection, the device it proved
// and the connection's id. The transport builds it (rpc, once per request,
// from the connection's attachment); nothing a client sends can set it.
//
// Every token resolution takes one, explicitly, and checkSession enforces the
// surface rules with it: a token minted for a remote device works only on a
// remote connection that proved that device and owns the session, and a
// token minted locally works only locally.
type Caller struct {
	Surface Surface
	// DeviceID is the enrolled device the connection proved; 0 when it has
	// proved none.
	DeviceID int64
	// ConnID is the remote connection's id (remote.Peer.ConnID).
	ConnID string
}

// LocalCaller is the caller of every request that did not arrive over the
// remote listener.
var LocalCaller = Caller{Surface: SurfaceLocal}

type callerKey struct{}

// WithCaller returns ctx carrying c, for the auth calls made on its behalf.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom is the caller ctx carries, or LocalCaller when it carries none.
//
// Absent means local because the remote surface has exactly one way in: the
// rpc server, which sets the caller for every request it dispatches, before
// the handler runs. Every other way to reach these methods (an in-process
// frontend, the web gateway's own connection, a test) is local.
func CallerFrom(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return LocalCaller
}

// reader is where a resolution reads: the pool, under a context, or an open
// transaction. checkSession runs on whichever it is given, so a check made
// inside a transaction sees what that transaction sees.
type reader struct {
	ctx context.Context
	tx  *dao.Transaction
}

func (s *Service) sessionsOn(r reader) dao.DAO[*meta.Session, meta.SessionField, int64] {
	if r.tx != nil {
		return s.store.Sessions.On(r.tx)
	}
	return s.store.Sessions.OnCtx(r.ctx)
}

func (s *Service) usersOn(r reader) dao.DAO[*meta.User, meta.UserField, int64] {
	if r.tx != nil {
		return s.store.Users.On(r.tx)
	}
	return s.store.Users.OnCtx(r.ctx)
}

func (s *Service) devicesOn(r reader) dao.DAO[*meta.RemoteDevice, meta.RemoteDeviceField, int64] {
	if r.tx != nil {
		return s.store.RemoteDevices.On(r.tx)
	}
	return s.store.RemoteDevices.OnCtx(r.ctx)
}

func (s *Service) sshKeysOn(r reader) dao.DAO[*meta.UserSSHKey, meta.UserSSHKeyField, int64] {
	if r.tx != nil {
		return s.store.SSHKeys.On(r.tx)
	}
	return s.store.SSHKeys.OnCtx(r.ctx)
}
