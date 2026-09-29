package rpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/logger"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/remote"
)

// sessClientVersion is what the client's sys.hello said it is (its "name"
// and "version"), for the device-enrollment event.
const sessClientVersion = "client-version"

// registerRemoteSignIn registers remote.attest. auth.login's remote branch
// is remoteLogin.
func (s *Server) registerRemoteSignIn() {
	// remote.attest(device_pub, sig) proves the connection's device before
	// the passphrase is sent. sig is the device key's signature over
	// remote.AttestMessage for this connection, built here from the
	// connection, never from the client. The reply says whether the device
	// is enrolled, and when its key was made; a device that is not is
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
		return map[string]any{"enrolled": at.DeviceID != 0, "key_created_at": at.KeyCreatedAt}, nil
	})
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
	release := func() {}
	if s.eng != nil {
		rel, err := s.eng.AdmitRemoteSession()
		if errors.Is(err, exec.ErrRemoteAdmissionClosed) {
			return nil, &golibrpc.Error{Code: CodeServerRestarting,
				Message: "the server is restarting; sign in again shortly"}
		}
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		release = rel
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
	if !peer.SignIn(res.SessionID, res.DeviceID) {
		// The gate admits one sign-in per connection; a second session is
		// never left behind if that ever fails.
		release()
		if _, derr := s.auth.DetachRemoteSession(0, peer.ConnID, 0); derr != nil {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote: revoking a second session on %s: %v", peer.ConnID, derr))
		}
		return nil, s.remoteRefusal(req, peer, auth.DenialProtocolViolation)
	}
	peer.OnEnd(func() {
		release()
		extra, derr := s.auth.DetachRemoteSession(res.SessionID, peer.ConnID, s.grace)
		if derr != nil {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote: the close write for session %d failed; it stays attached "+
				"to the ended connection until it is taken up, expires or the daemon restarts: %v", res.SessionID, derr))
		}
		if extra > 0 {
			s.logger.Log(logger.SeverityError, fmt.Sprintf("remote_extra_session_on_close: %d session(s) besides %d were attached to %s; revoked",
				extra, res.SessionID, peer.ConnID))
		}
	})
	if s.remoteSignedIn != nil {
		s.remoteSignedIn(ip)
	}
	return map[string]any{"token": res.Token, "user": identMap(res.Identity), "enrolled": res.Enrolled}, nil
}

// remoteRefusal counts a remote connection's refused proof or sign-in and
// returns the refusal to answer it with. Which check failed is not said.
func (s *Server) remoteRefusal(req *golibrpc.Request, peer *remote.Peer, reason auth.DenialReason) error {
	if s.remoteDenial(req.Session, peer, string(reason)) {
		peer.Hangup()
	}
	return &golibrpc.Error{Code: CodeRemoteDenied, Message: "remote sign-in refused"}
}
