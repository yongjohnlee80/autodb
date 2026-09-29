package remote

import "sync"

// Registry holds the live remote connections, so they can be ended from
// outside: every one when Remote Control is turned off, or those of one SSH
// key or one user when it is revoked or disabled. A connection is registered
// when the listener hands it over and removed when its SSH session ends.
//
// It outlives any one listener: a listener restarted by its supervisor does
// not end the connections the previous one accepted, and turning Remote
// Control off must still reach them.
type Registry struct {
	mu    sync.Mutex
	peers map[string]*Peer // by ConnID
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{peers: map[string]*Peer{}} }

func (r *Registry) add(p *Peer) {
	r.mu.Lock()
	r.peers[p.ConnID] = p
	r.mu.Unlock()
}

func (r *Registry) remove(connID string) {
	r.mu.Lock()
	delete(r.peers, connID)
	r.mu.Unlock()
}

// Len is how many remote connections are live.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

// Close hangs up every live remote connection match accepts, and returns how
// many. The hangup is graceful (Peer.Hangup): what the server already wrote
// is sent first. Call it after the change that justifies it (a revocation, the
// switch turned off) has been committed, so a reconnect is judged by the new
// state.
func (r *Registry) Close(match func(*Peer) bool) int {
	r.mu.Lock()
	var hit []*Peer
	for _, p := range r.peers {
		if match(p) {
			hit = append(hit, p)
		}
	}
	r.mu.Unlock()
	for _, p := range hit {
		p.Hangup()
	}
	return len(hit)
}

// All matches every connection.
func All(*Peer) bool { return true }

// BySSHKey matches the connections authenticated with one registered key.
func BySSHKey(keyID int64) func(*Peer) bool {
	return func(p *Peer) bool { return p.SSHKeyID == keyID }
}

// ByConn matches the one connection with id connID.
func ByConn(connID string) func(*Peer) bool {
	return func(p *Peer) bool { return p.ConnID == connID }
}

// ByUser matches the connections of one user.
func ByUser(userID int64) func(*Peer) bool {
	return func(p *Peer) bool { return p.UserID == userID }
}
