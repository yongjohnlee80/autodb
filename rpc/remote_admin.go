package rpc

import (
	"context"
	"errors"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/remote"
	"github.com/yongjohnlee80/autodb/core/remotectl"
)

// RemoteController is Remote Control as the RPC server uses it: its status
// and its switch (remotectl.Control).
type RemoteController interface {
	Status() remotectl.Status
	Set(ctx context.Context, on bool, byUserID int64, ip string) error
}

// WithRemoteControl sets the daemon's Remote Control, for
// remote.control_get/set. Without it they answer that it is not available.
func WithRemoteControl(c RemoteController) Option {
	return func(o *options) { o.remoteControl = c }
}

// WithRemoteUnblock sets how an admin lifts an address block
// (RemoteLimiter.Unblock). Without it remote.blocks_unblock answers that it
// is not available.
func WithRemoteUnblock(fn func(ctx context.Context, byUserID int64, prefix, ip string) error) Option {
	return func(o *options) { o.remoteUnblock = fn }
}

// endRemote ends the live remote connections match accepts, after the change
// that justifies it has committed.
func (s *Server) endRemote(match func(*remote.Peer) bool) {
	if s.remoteClose != nil {
		s.remoteClose(match)
	}
}

// endRemoteAnswering is endRemote for a request that may be ending its own
// connection: that one ends after its reply, as a sign-out does, so the
// client learns the revocation happened (and discards the device key it
// holds rather than reconnecting with it). The others end now.
func (s *Server) endRemoteAnswering(req *golibrpc.Request, match func(*remote.Peer) bool) {
	own, _ := remotePeer(req.Session)
	s.endRemote(func(p *remote.Peer) bool { return p != own && match(p) })
	if own != nil && match(own) {
		own.EndAfterReply(remoteHangupBackstop)
	}
}

// adminErr maps the remote administration refusals onto the wire.
func (s *Server) adminErr(req *golibrpc.Request, err error) error {
	switch {
	case errors.Is(err, auth.ErrSSHKeyInvalid), errors.Is(err, auth.ErrSSHKeyTaken):
		return &golibrpc.Error{Code: golibrpc.CodeInvalidParams, Message: err.Error()}
	case errors.Is(err, auth.ErrNotFound):
		return &golibrpc.Error{Code: CodeNotFound, Message: "no such key or device"}
	}
	return s.wireErrFor(req, err)
}

func keyMap(k auth.SSHKeyInfo) map[string]any {
	m := map[string]any{
		"id": k.ID, "user_id": k.UserID, "user": k.User, "label": k.Label,
		"fingerprint": k.Fingerprint, "type": k.Type, "added_by": k.AddedBy,
		"created_at": k.CreatedAt, "last_used_at": k.LastUsedAt, "device": nil,
	}
	if k.Device != nil {
		m["device"] = deviceMap(*k.Device)
	}
	return m
}

func deviceMap(d auth.DeviceInfo) map[string]any {
	return map[string]any{
		"id": d.ID, "ssh_key_id": d.SSHKeyID, "user_id": d.UserID, "user": d.User,
		"fingerprint": d.Fingerprint, "enrolled_at": d.EnrolledAt, "enrolled_ip": d.EnrolledIP,
		"key_created_at": d.KeyCreatedAt, "last_seen_at": d.LastSeenAt, "last_ip": d.LastIP,
		"revoked_at": d.RevokedAt,
	}
}

// registerRemoteAdmin registers the administration of remote access: SSH
// keys, devices, blocked addresses, the Remote Control switch and the remote
// activity. Every one is served on both surfaces; who may do what is the
// core's (auth, exec), per the caller's own role.
func (s *Server) registerRemoteAdmin() {
	// remote.ssh_key_add(token, user_id, public_key, label, passphrase):
	// user_id 0 is the caller. Adding to one's own profile needs the
	// passphrase; an admin adding to another's does not, and passes "".
	s.handle("remote.ssh_key_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 5); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		pub, err := argStr(req.Params, 2, "public_key")
		if err != nil {
			return nil, err
		}
		label, err := argStr(req.Params, 3, "label")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 4, "passphrase")
		if err != nil {
			return nil, err
		}
		k, err := s.auth.AddSSHKey(ctx, token, userID, pub, label, pass, peerIP(req))
		if err != nil {
			return nil, s.adminErr(req, err)
		}
		return keyMap(k), nil
	})
	// remote.ssh_key_list(token, user_id): 0 the caller; -1 everyone (admin).
	s.handle("remote.ssh_key_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, userID, err := tokenAndID(req, "user_id")
		if err != nil {
			return nil, err
		}
		keys, err := s.auth.ListSSHKeys(ctx, token, userID)
		if err != nil {
			return nil, s.adminErr(req, err)
		}
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyMap(k))
		}
		return out, nil
	})
	s.handle("remote.ssh_key_label", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		keyID, err := argInt(req.Params, 1, "key_id")
		if err != nil {
			return nil, err
		}
		label, err := argStr(req.Params, 2, "label")
		if err != nil {
			return nil, err
		}
		return nil, s.adminErr(req, s.auth.LabelSSHKey(ctx, token, keyID, label, peerIP(req)))
	})
	// remote.ssh_key_revoke(token, key_id) revokes the key, its device and
	// their sessions, then ends the key's live connections: the caller's own
	// after its reply.
	s.handle("remote.ssh_key_revoke", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, keyID, err := tokenAndID(req, "key_id")
		if err != nil {
			return nil, err
		}
		if err := s.auth.RevokeSSHKey(ctx, token, keyID, peerIP(req)); err != nil {
			return nil, s.adminErr(req, err)
		}
		s.endRemoteAnswering(req, remote.BySSHKey(keyID))
		return nil, nil
	})
	// remote.device_list(token, user_id, with_revoked).
	s.handle("remote.device_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		withRevoked, ok := req.Params[2].(bool)
		if !ok {
			return nil, invalid("argument 2 (with_revoked): want a boolean, got %T", req.Params[2])
		}
		devs, err := s.auth.ListDevices(ctx, token, userID, withRevoked)
		if err != nil {
			return nil, s.adminErr(req, err)
		}
		out := make([]any, 0, len(devs))
		for _, d := range devs {
			out = append(out, deviceMap(d))
		}
		return out, nil
	})
	// remote.device_revoke(token, device_id) revokes the device and its
	// sessions, then ends its live connections: the caller's own after its
	// reply.
	s.handle("remote.device_revoke", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, deviceID, err := tokenAndID(req, "device_id")
		if err != nil {
			return nil, err
		}
		if err := s.auth.RevokeDevice(ctx, token, deviceID, peerIP(req)); err != nil {
			return nil, s.adminErr(req, err)
		}
		s.endRemoteAnswering(req, remote.ByDevice(deviceID))
		return nil, nil
	})
	s.handle("remote.blocks_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		blocks, err := s.auth.ListBlocks(ctx, token)
		if err != nil {
			return nil, s.adminErr(req, err)
		}
		now := time.Now().Unix()
		out := make([]any, 0, len(blocks))
		for _, b := range blocks {
			out = append(out, map[string]any{
				"prefix": b.Prefix, "failures": b.Failures, "last_failure_at": b.LastFailureAt,
				"blocked_until": b.BlockedUntil, "blocked": b.BlockedUntil > now, "reason": b.Reason,
			})
		}
		return out, nil
	})
	s.handle("remote.blocks_unblock", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		prefix, err := argStr(req.Params, 1, "prefix")
		if err != nil {
			return nil, err
		}
		admin, err := s.auth.RequireAdmin(ctx, token)
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		if s.remoteUnblock == nil {
			return nil, &golibrpc.Error{Code: CodeRemoteUnavailable, Message: "remote access is not available on this server"}
		}
		return nil, s.wireErrFor(req, s.remoteUnblock(ctx, admin.UserID(), prefix, peerIP(req)))
	})
	s.handle("remote.control_get", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		if _, err := s.auth.RequireAdmin(ctx, token); err != nil {
			return nil, s.wireErrFor(req, err)
		}
		if s.remoteControl == nil {
			return nil, &golibrpc.Error{Code: CodeRemoteUnavailable, Message: "remote access is not available on this server"}
		}
		return controlMap(s.remoteControl.Status()), nil
	})
	// remote.control_set(token, on) turns Remote Control on or off. Off ends
	// every live remote connection, the caller's own too when it is one.
	s.handle("remote.control_set", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		on, ok := req.Params[1].(bool)
		if !ok {
			return nil, invalid("argument 1 (on): want a boolean, got %T", req.Params[1])
		}
		admin, err := s.auth.RequireAdmin(ctx, token)
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		if s.remoteControl == nil {
			return nil, &golibrpc.Error{Code: CodeRemoteUnavailable, Message: "remote access is not available on this server"}
		}
		if err := s.remoteControl.Set(ctx, on, admin.UserID(), peerIP(req)); err != nil {
			return nil, s.wireErrFor(req, err)
		}
		return controlMap(s.remoteControl.Status()), nil
	})
	// remote.activity_search(token, filter): the remote connection events,
	// as audit.search pages them. An admin sees all three kinds; anyone else
	// their own enrollments and new addresses.
	s.handle("remote.activity_search", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		f, err := auditFilterFrom(req.Params[1])
		if err != nil {
			return nil, err
		}
		page, err := s.eng.SearchRemoteActivity(ctx, token, f)
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		return auditPageMap(page), nil
	})
}

// tokenAndID reads (token, id) arguments.
func tokenAndID(req *golibrpc.Request, name string) (string, int64, error) {
	if err := exactArgs(req.Params, 2); err != nil {
		return "", 0, err
	}
	token, err := argStr(req.Params, 0, "token")
	if err != nil {
		return "", 0, err
	}
	id, err := argInt(req.Params, 1, name)
	return token, id, err
}

func controlMap(st remotectl.Status) map[string]any {
	next := int64(0)
	if !st.NextTry.IsZero() {
		next = st.NextTry.Unix()
	}
	return map[string]any{
		"on": st.On, "state": st.State, "addr": st.Addr, "host_key_fp": st.HostKeyFP,
		"error": st.Err, "next_try": next, "live": int64(st.Live),
	}
}
