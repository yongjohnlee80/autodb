package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// sessClientVersion is what the client's sys.hello said it is (its "name"
// and "version"), for the device-enrollment event.
const sessClientVersion = "client-version"

// registerRemoteSignIn registers remote.attest, remote.resume and
// remote.rotate_device. auth.login's remote branch is remoteLogin.
func (s *Server) registerRemoteSignIn() {
	// remote.attest(device_pub, sig) proves the connection's device before
	// the passphrase is sent. sig is the device key's signature over
	// remote.AttestMessage for this connection, built here from the
	// connection, never from the client. The reply says whether the device
	// is enrolled, its id (what a rotation signs), when its key was made and
	// whether it is due for rotation; a device that is not enrolled is
	// enrolled by the sign-in that follows.
	s.handle("remote.attest", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		peer, isRemote := remotePeer(req.Session)
		if !isRemote {
			return nil, &golibrpc.Error{Code: CodeRemoteRefused,
				Message: "remote.attest is for remote connections"}
		}
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		pub, err := argBin(req.Params, 0, "device_pub")
		if err != nil {
			return nil, err
		}
		sig, err := argBin(req.Params, 1, "sig")
		if err != nil {
			return nil, err
		}
		if peer == nil {
			return nil, s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
		}
		msg := remote.AttestMessage(peer.SessionID, peer.HostKeyFP, peer.SSHKeyFP, pub)
		at, err := s.auth.AttestRemote(ctx, peer.SSHKeyID, msg, pub, sig)
		if err != nil {
			if reason, ok := auth.DenialFor(err); ok {
				return nil, s.remoteRefusal(req, peer, reason)
			}
			return nil, s.wireErrFor(req, err)
		}
		proved := false
		if at.DeviceID != 0 {
			proved = peer.Attest(at.DeviceID)
		} else {
			proved = peer.Stage(pub)
		}
		if !proved {
			// A second proof on one connection.
			return nil, s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
		}
		due := at.DeviceID != 0 && time.Since(time.Unix(at.KeyCreatedAt, 0)) > s.deviceKeyMaxAge
		return map[string]any{"enrolled": at.DeviceID != 0, "device_id": at.DeviceID,
			"key_created_at": at.KeyCreatedAt, "rotate_due": due}, nil
	})

	// remote.resume(token) takes up this device's session on a new connection
	// within the reconnect grace, without the passphrase: after sys.hello and
	// remote.attest of an ENROLLED device. A token that is not this device's
	// is a counted denial. One that is, but can no longer be taken up, is
	// CodeResumeUnavailable: uncounted, and the connection stays open to
	// sign in with the passphrase.
	s.handle("remote.resume", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		peer, isRemote := remotePeer(req.Session)
		if !isRemote {
			return nil, &golibrpc.Error{Code: CodeRemoteRefused,
				Message: "remote.resume is for remote connections"}
		}
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		if peer == nil || peer.Device() == 0 {
			// Resuming needs an enrolled device proved first; a device to
			// enroll signs in with the passphrase.
			return nil, s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
		}
		release, rerr := s.admitRemote(req)
		if rerr != nil {
			return nil, rerr
		}
		ip := remoteIP(req.Session, peer)
		res, err := s.auth.ResumeRemote(ctx, auth.RemoteResume{DeviceID: peer.Device(), ConnID: peer.ConnID}, token, ip)
		if err != nil {
			release()
			if errors.Is(err, auth.ErrRemoteResumeUnavailable) {
				return nil, &golibrpc.Error{Code: CodeResumeUnavailable,
					Message: "that session can no longer be resumed; sign in"}
			}
			if reason, ok := auth.DenialFor(err); ok {
				return nil, s.remoteRefusal(req, peer, reason)
			}
			return nil, s.wireErrFor(req, err)
		}
		if err := s.signedIn(req, peer, res.SessionID, peer.Device(), release, ip); err != nil {
			return nil, err
		}
		if res.TookOver != "" && s.remoteClose != nil {
			s.remoteClose(remote.ByConn(res.TookOver))
		}
		return map[string]any{"user": identMap(res.Identity)}, nil
	})

	// remote.rotate_device(token, new_pub, sig_old, sig_new) replaces this
	// connection's device key. Both keys sign remote.RotateMessage for this
	// connection, this device, the current key and the new one. The reply is
	// the new key's creation time.
	s.handle("remote.rotate_device", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		peer, isRemote := remotePeer(req.Session)
		if !isRemote || peer == nil {
			return nil, &golibrpc.Error{Code: CodeRemoteRefused,
				Message: "remote.rotate_device is for remote connections"}
		}
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		newPub, err := argBin(req.Params, 1, "new_pub")
		if err != nil {
			return nil, err
		}
		sigOld, err := argBin(req.Params, 2, "sig_old")
		if err != nil {
			return nil, err
		}
		sigNew, err := argBin(req.Params, 3, "sig_new")
		if err != nil {
			return nil, err
		}
		msgFor := func(oldPub []byte) []byte {
			return remote.RotateMessage(peer.SessionID, peer.Device(), oldPub, newPub)
		}
		at, err := s.auth.RotateDevice(ctx, token, msgFor, newPub, sigOld, sigNew, remoteIP(req.Session, peer))
		if errors.Is(err, auth.ErrRemoteDeviceProofInvalid) {
			return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
				Message: "the rotation proof does not verify; nothing changed"}
		}
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		return map[string]any{"key_created_at": at}, nil
	})
}

// admitRemote claims the idle gate's remote admission for a sign-in or a
// resume, or refuses it as restarting (uncounted).
func (s *Server) admitRemote(req *golibrpc.Request) (func(), error) {
	if s.eng == nil {
		return func() {}, nil
	}
	rel, err := s.eng.AdmitRemoteSession()
	if errors.Is(err, exec.ErrRemoteAdmissionClosed) {
		return nil, &golibrpc.Error{Code: CodeServerRestarting,
			Message: "the server is restarting; sign in again shortly"}
	}
	if err != nil {
		return nil, s.wireErrFor(req, err)
	}
	return rel, nil
}

// signedIn finishes a sign-in or a resume that committed: the connection is
// signed in to its one session, bound to device; when it ends, its admission
// is released and its session detached for the reconnect grace; and its
// address's run of refusals ends.
func (s *Server) signedIn(req *golibrpc.Request, peer *remote.Peer, session, device int64, release func(), ip string) error {
	if !peer.SignIn(session, device) {
		// The gate admits one sign-in per connection; a second session is
		// never left behind if that ever fails.
		release()
		if _, derr := s.auth.DetachRemoteSession(0, peer.ConnID, 0); derr != nil {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote: revoking a second session on %s: %v", peer.ConnID, derr))
		}
		return s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
	}
	peer.OnEnd(func() {
		release()
		extra, derr := s.auth.DetachRemoteSession(session, peer.ConnID, s.grace)
		if derr != nil {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote: the close write for session %d failed; it stays attached "+
				"to the ended connection until it is taken up, expires or the daemon restarts: %v", session, derr))
		}
		if extra > 0 {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote_extra_session_on_close: %d session(s) besides %d were attached to %s; revoked",
				extra, session, peer.ConnID))
		}
	})
	if s.remoteSignedIn != nil {
		s.remoteSignedIn(ip)
	}
	return nil
}

// remoteLogin is auth.login on a remote connection: auth.LoginRemote with
// what the connection proved, under the idle gate's remote admission.
//
// Admission is claimed before the sign-in commits and held until the
// connection ends, so a sign-in racing an idle restart is either counted or
// refused (CodeServerRestarting, uncounted). A refused sign-in is a counted
// denial, and the connection ends after its reply. On success the connection
// is signed in to its one session, its address's run of refusals ends, and
// when the connection ends its session is detached for the reconnect grace.
func (s *Server) remoteLogin(ctx context.Context, req *golibrpc.Request, peer *remote.Peer, name, pass string) (any, error) {
	if peer == nil {
		return nil, s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
	}
	release, rerr := s.admitRemote(req)
	if rerr != nil {
		return nil, rerr
	}
	clientVersion, _ := req.Session.Value(sessClientVersion).(string)
	ip := remoteIP(req.Session, peer)
	res, err := s.auth.LoginRemote(ctx, auth.RemoteLogin{
		SSHKeyID: peer.SSHKeyID, OwnerID: peer.UserID, SSHKeyFP: peer.SSHKeyFP,
		DeviceID: peer.Device(), PendingPub: peer.Pending(),
		ConnID: peer.ConnID, ClientVersion: clientVersion,
	}, name, pass, ip)
	if err != nil {
		release()
		if reason, ok := auth.DenialFor(err); ok {
			return nil, s.remoteRefusal(req, peer, reason)
		}
		return nil, s.wireErrFor(req, err)
	}
	if err := s.signedIn(req, peer, res.SessionID, res.DeviceID, release, ip); err != nil {
		return nil, err
	}
	newDevices := make([]any, 0, len(res.NewDevices))
	for _, d := range res.NewDevices {
		newDevices = append(newDevices, deviceMap(d))
	}
	return map[string]any{"token": res.Token, "user": identMap(res.Identity), "enrolled": res.Enrolled,
		"new_devices": newDevices}, nil
}

// remoteRefusal counts a remote connection's refused proof or sign-in and
// returns the refusal to answer it with. Which check failed is not said.
func (s *Server) remoteRefusal(req *golibrpc.Request, peer *remote.Peer, reason auth.DenialReason) error {
	if s.remoteDenial(req.Session, peer, string(reason)) {
		peer.Hangup()
	}
	return &golibrpc.Error{Code: CodeRemoteDenied, Message: "remote sign-in refused"}
}
