package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// Session is the TUI's ONLY path to the core (the client
// seam, even in-process): a golib rpc.Client plus the autodb handshake,
// login state, and typed projections of the method surface. The reconnect
// loop is driven by the client's Done()/Err() terminal signal; a changed
// server instance across reconnects drops cached state (tokens persist in
// the meta store, but the master key does not survive a restart —
// public ErrLocked results are treated as the login-required transition).
//
// All mutable state is guarded by mu: worker goroutines issue calls and
// adopt logins while the loop goroutine reads Token/User/Gen. Every state
// transition is generation-conditioned — a result from an old connection
// (or a CodeAuth for a token that has since been replaced) can never
// clobber newer state.
type Session struct {
	addr  string
	log   logger.Logger
	spawn func() (logHint string, err error) // start `autodb --serve`; nil = never spawn

	mu         sync.Mutex
	client     *golibrpc.Client
	instance   string
	network    string // "unix" (default) or "tcp"
	version    string
	serverPID  int64
	serverAddr string
	token      string
	user       UserInfo
	gen        uint64 // state epoch; bumps when a (re)connect/disconnect BEGINS
}

// spawnProbeWindow bounds how long Connect keeps dialing after the first
// failure before giving up (a spawned server that exits
// early keeps refusing dials, so the bounded window detects it too).
const spawnProbeWindow = 15 * time.Second

// UserInfo is the logged-in identity as reported by the server.
type UserInfo struct {
	ID   int64
	Name string
	Role string
}

// NewSession prepares an unconnected session.
func NewSession(addr string, log logger.Logger, spawn func() (string, error)) *Session {
	return NewSessionOn("tcp", addr, log, spawn)
}

// NewSessionOn is NewSession on an explicit network — "unix" (the
// default endpoint) or "tcp" (a configured port). The frontend does not
// choose this: config.Server.Endpoint() resolves it once, and both the
// listener and every dial follow that one answer.
func NewSessionOn(network, addr string, log logger.Logger, spawn func() (string, error)) *Session {
	if log == nil {
		log = logger.Nop{}
	}
	if network == "" {
		network = "tcp"
	}
	return &Session{network: network, addr: addr, log: log, spawn: spawn}
}

// Gen reports the state epoch (stale-task filtering in the UI).
func (s *Session) Gen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Token reports the bearer token ("" = not logged in).
func (s *Session) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// User reports the logged-in identity.
func (s *Session) User() UserInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user
}

// IsAdmin reports whether the logged-in user is an admin.
func (s *Session) IsAdmin() bool { return s.User().Role == "admin" }

// CanSpawn reports whether this session may start a replacement daemon.
//
// It is the capability behind SPC X. `restartServer` shuts the daemon down and
// relies on the disconnect watcher to start a fresh one, and that replacement
// comes from here — so an action that stops the daemon has to ask this first.
// A nil spawner is the deliberate state of a client_only config, which
// install_frontdoor.sh writes so a config handed to somebody who is not the
// operator cannot become what listens.
func (s *Session) CanSpawn() bool { return s.spawn != nil }

// Connected reports whether a live client is installed.
func (s *Session) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil
}

// ServerStatus describes the backend an operator is talking to: its pid
// and the address it listens on ("" / 0 when not connected, or when the
// server predates the handshake reporting them).
func (s *Session) ServerStatus() (pid int64, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return 0, ""
	}
	addr = s.serverAddr
	if addr == "" {
		addr = s.addr // what we dialed, when the server does not report
	}
	return s.serverPID, addr
}

// ServerVersion reports the connected server's version string.
func (s *Session) ServerVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Done exposes the underlying client's terminal signal (nil-safe: an
// unconnected session returns a nil channel, which never fires).
func (s *Session) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	return s.client.Done()
}

// Err reports the terminal cause after Done fires.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	return s.client.Err()
}

// Bound is a connection-epoch-pinned view of the Session: every call goes
// to the client and token captured at Bind time. UI actions bind at
// ISSUANCE (on the loop goroutine), so queued work that a reconnect
// supersedes is refused before the RPC — and even past the refusal's
// race window it can only reach the pinned OLD client (already closed by
// the transition), never the new server. Mutations cannot cross epochs.
type Bound struct {
	s     *Session
	cli   *golibrpc.Client
	gen   uint64
	token string
	// user is pinned WITH the token, because they are two halves of one
	// identity. A review found the PAT card reading the live session instead:
	// the mint used this pinned token while the card read m.session.User()
	// afterwards, so a login switch between the two rendered one person's
	// token in a DSN naming another. A same-connection switch does not bump
	// gen, so the epoch could not catch it -- the identity has to travel with
	// the credential.
	user UserInfo
}

// Bind pins the current epoch. Call it where the user's intent forms —
// the action issuance point — not inside the worker.
func (s *Session) Bind() *Bound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Bound{s: s, cli: s.client, gen: s.gen, token: s.token, user: s.user}
}

// Gen reports the pinned epoch (result tagging at issuance sites).
func (b *Bound) Gen() uint64 { return b.gen }

// User reports the identity pinned alongside the token, which is the account
// whose credential this Bound acts with. Anything that RENDERS an identity for
// work done through a Bound must take it from here, not from the live session.
func (b *Bound) User() UserInfo { return b.user }

// errSuperseded refuses work whose issuing epoch has been replaced.
var errSuperseded = errors.New("tui: connection changed since this action was issued")

// ensure rejects the call before the RPC when the view is unusable.
func (b *Bound) ensure() error {
	if b.cli == nil {
		return errors.New("tui: not connected")
	}
	b.s.mu.Lock()
	current := b.s.gen == b.gen
	b.s.mu.Unlock()
	if !current {
		return errSuperseded
	}
	return nil
}

// Connect implements the FE contract: dial; on refusal spawn
// `--serve` (when a spawner is configured) and retry with backoff inside a
// bounded probe window; then hello at the current Protocol. It reports
// whether the server INSTANCE changed versus the previous connection — the
// caller drops server-derived UI state and re-prompts login on a change.
//
// The epoch bumps as soon as the reconnect BEGINS: in-flight results,
// stale disconnect watchers, and CodeAuth clears from the old connection
// are all invalidated before the old client is even closed.
func (s *Session) Connect(ctx context.Context) (instanceChanged bool, err error) {
	s.mu.Lock()
	old := s.client
	s.client = nil
	s.gen++
	myGen := s.gen
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	var cli *golibrpc.Client
	backoff := 100 * time.Millisecond
	spawned := false
	logHint := ""
	var deadline time.Time
	for {
		cli, err = golibrpc.Dial(ctx, s.addr, msgpackrpc.New(nil),
			golibrpc.ClientNetwork(s.network))
		if err == nil {
			break
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(spawnProbeWindow)
		}
		if s.spawn != nil && !spawned {
			s.log.Log(logger.SeverityInfo, map[string]any{
				"tui": "session", "event": "spawning server", "addr": s.addr,
			})
			hint, serr := s.spawn()
			if serr != nil {
				return false, fmt.Errorf("spawn autodb --serve: %w", serr)
			}
			logHint = hint
			spawned = true
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("connect %s: server did not answer within %s (last: %v)",
				s.addr, spawnProbeWindow, err)
			if logHint != "" {
				msg += " — check " + logHint
			}
			return false, errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("connect %s: %w (last: %v)", s.addr, ctx.Err(), err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}

	res, err := cli.Call(ctx, "sys.hello", map[string]any{
		"protocol": rpc.Protocol, "name": "autodb-tui",
	})
	if err != nil {
		_ = cli.Close()
		var re *golibrpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeProtocolMismatch {
			// Client and server are different builds. Which one is stale
			// decides what to do, and BOTH directions happen: the shared
			// server outlives frontends (so it is usually the old one),
			// but restarting it from a rebuilt binary makes the running
			// TUI the old one instead. The server's message carries both
			// numbers; add the instruction.
			hint := "stop the running server so a current one starts: " +
				"pkill -f 'autodb --serve'"
			if serverProto := protocolOf(re.Message); serverProto > rpc.Protocol {
				hint = "this frontend is the older build — quit and relaunch it"
			}
			return false, fmt.Errorf("%s — %s", re.Message, hint)
		}
		return false, fmt.Errorf("handshake: %w", err)
	}
	m, _ := res.(map[string]any)
	inst, _ := m["instance"].(string)
	ver, _ := m["version"].(string)
	pid, _ := m["pid"].(int64)
	srvAddr, _ := m["addr"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != myGen {
		// A newer Connect/Disconnect superseded this attempt while it was
		// dialing; its client must not be installed over the newer state.
		_ = cli.Close()
		return false, errors.New("tui: connect superseded by a newer transition")
	}
	instanceChanged = s.instance != "" && s.instance != inst
	if instanceChanged {
		// A new server process: the master key is locked again and every
		// cached assumption is stale.
		s.token = ""
		s.user = UserInfo{}
	}
	s.client = cli
	s.instance = inst
	s.version = ver
	s.serverPID = pid
	s.serverAddr = srvAddr
	return instanceChanged, nil
}

// Disconnect closes the client and bumps the epoch (leader `x`). The
// token is kept: reconnecting to the SAME instance stays logged in;
// an instance change on reconnect drops it as usual.
func (s *Session) Disconnect() {
	s.mu.Lock()
	old := s.client
	s.client = nil
	s.gen++
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// protocolOf reads the server's protocol out of a mismatch message
// ("protocol mismatch: client N, server M"); 0 when it cannot.
func protocolOf(msg string) int64 {
	i := strings.LastIndex(msg, "server ")
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(msg[i+len("server "):]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Close shuts the underlying client down.
func (s *Session) Close() { s.Disconnect() }

// call issues a tokenless method (hello aside, only auth.needs_bootstrap).
func (b *Bound) call(ctx context.Context, method string, params ...any) (any, error) {
	if err := b.ensure(); err != nil {
		return nil, err
	}
	return b.cli.Call(ctx, method, params...)
}

// authed is the token-first chokepoint over the PINNED client and token;
// on a public CodeAuth failure it invalidates the login state — but ONLY
// if this exact token is still current in this exact epoch, so a stale
// failure can never clear a newer login.
func (b *Bound) authed(ctx context.Context, method string, extra ...any) (any, error) {
	if err := b.ensure(); err != nil {
		return nil, err
	}
	params := append([]any{b.token}, extra...)
	res, err := b.cli.Call(ctx, method, params...)
	if err != nil {
		var re *golibrpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeAuth {
			// Stale token or locked store: the UI's one recovery for both
			// is the login flow (the Model watches the token-empty edge).
			b.s.mu.Lock()
			if b.s.gen == b.gen && b.s.token == b.token {
				b.s.token = ""
				b.s.user = UserInfo{}
			}
			b.s.mu.Unlock()
		}
		return nil, err
	}
	return res, nil
}

// --- typed projections -----------------------------------------------------

// NeedsBootstrap queries the server to determine whether first-time admin setup is required.
func (b *Bound) NeedsBootstrap(ctx context.Context) (bool, error) {
	res, err := b.call(ctx, "auth.needs_bootstrap")
	if err != nil {
		return false, err
	}
	needs, _ := res.(bool)
	return needs, nil
}

// GlobalIPAdmitted asks whether the GLOBAL allowlist alone admits ip.
//
// Tokenless, because the one caller is the web gateway deciding whether an
// address may perform the first-admin bootstrap — at which point no account
// and therefore no token exists. Ordinary login uses IPAdmitted, which
// consults both layers against a proven identity.
func (b *Bound) GlobalIPAdmitted(ctx context.Context, ip string) (bool, error) {
	res, err := b.call(ctx, "auth.global_ip_admitted", ip)
	if err != nil {
		return false, err
	}
	admitted, _ := res.(bool)
	return admitted, nil
}

// adoptLogin installs a login result unless the epoch moved on.
func (s *Session) adoptLogin(res any, gen uint64) {
	m, _ := res.(map[string]any)
	tok, _ := m["token"].(string)
	var u UserInfo
	if um, ok := m["user"].(map[string]any); ok {
		u = UserInfo{ID: mI(um, "id"), Name: mS(um, "name"), Role: mS(um, "role")}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return // logged into an old connection; the result is void
	}
	s.token = tok
	s.user = u
}

// Bootstrap creates the initial root administrator account and adopts the returned session.
func (b *Bound) Bootstrap(ctx context.Context, name, pass string) error {
	res, err := b.call(ctx, "auth.bootstrap", name, pass)
	if err != nil {
		return err
	}
	b.s.adoptLogin(res, b.gen)
	return nil
}

// Login authenticates with a username and passphrase, updating the session on success.
func (b *Bound) Login(ctx context.Context, name, pass string) error {
	res, err := b.call(ctx, "auth.login", name, pass)
	if err != nil {
		return err
	}
	b.s.adoptLogin(res, b.gen)
	return nil
}

// LoginAt is Login with the admission address the CALLER observed — the web
// gateway's browser peer, which the daemon cannot see for itself.
//
// One call rather than a login followed by a separate admission question,
// because the two-call shape made a correct password do more work than an
// incorrect one and a refused caller could time the difference.
func (b *Bound) LoginAt(ctx context.Context, name, pass, admissionIP string) error {
	res, err := b.call(ctx, "auth.login_at", name, pass, admissionIP)
	if err != nil {
		return err
	}
	b.s.adoptLogin(res, b.gen)
	return nil
}

// Logout revokes the current session on the server and clears local credentials.
func (b *Bound) Logout(ctx context.Context) error {
	if b.token == "" {
		return nil
	}
	_, err := b.authed(ctx, "auth.logout")
	b.s.mu.Lock()
	if b.s.gen == b.gen && b.s.token == b.token {
		b.s.token = ""
		b.s.user = UserInfo{}
	}
	b.s.mu.Unlock()
	return err
}

// HistoryRow is one recorded execution (script history).
type HistoryRow struct {
	User      string
	Conn      string
	IP        string
	Script    string
	StartedAt string
	Duration  time.Duration
	RowCount  int64
	Status    string
	Error     string

	// Suspended came back on its own key rather than as a status value,
	// because status is the durability token and a suspended Execute did
	// commit. A daemon that does not send the key yields false, which reads as
	// "not suspended" -- the answer every pre-existing row deserves.
	Suspended bool
}

// History fetches recent script execution history records up to limit.
func (b *Bound) History(ctx context.Context, limit int64) ([]HistoryRow, error) {
	res, err := b.authed(ctx, "history.list", limit)
	if err != nil {
		return nil, err
	}
	var out []HistoryRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, HistoryRow{
			User: mS(m, "user"), Conn: mS(m, "connection"), IP: mS(m, "ip"),
			Script: mS(m, "script"), StartedAt: mS(m, "started_at"),
			Duration: time.Duration(mI(m, "duration_ms")) * time.Millisecond,
			RowCount: mI(m, "row_count"), Status: mS(m, "status"),
			Error: mS(m, "error"), Suspended: mB(m, "suspended"),
		})
	}
	return out, nil
}

// TxStatus is one transaction's resolved outcome.
//
// The server folds its transition log before it gets here, so this is the
// answer rather than the evidence: a consumer that had to fold the log itself
// would be keeping its own copy of the state machine, and copies drift.
type TxStatus struct {
	TxID     string
	State    string // opened|commit_started|unknown_pending|committed|rolled_back|outcome_unresolvable
	Reason   string
	ConnID   int64
	Terminal bool
	// Stuck is how long the transaction has been in its CURRENT state. It is
	// the number that decides whether to act, and the server computes it so
	// three clients cannot get three answers.
	Stuck time.Duration
}

// TxOutcome asks about one transaction. A transaction that is not this
// caller's answers exactly as one that never existed.
func (b *Bound) TxOutcome(ctx context.Context, txID string) (TxStatus, error) {
	res, err := b.authed(ctx, "tx.status", txID)
	if err != nil {
		return TxStatus{}, err
	}
	m, _ := res.(map[string]any)
	return txStatusOf(m), nil
}

// PendingTx lists this caller's UNRESOLVED transactions, oldest first —
// "what is stuck", which is the operator's question. It does not read the
// script history: history vanishes when [history].enabled is false, and a
// boundary-only BEGIN/COMMIT never had a row there to begin with.
func (b *Bound) PendingTx(ctx context.Context, limit int64) ([]TxStatus, error) {
	res, err := b.authed(ctx, "tx.status", "", limit)
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	var out []TxStatus
	for _, row := range asList(m["pending"]) {
		rm, _ := row.(map[string]any)
		out = append(out, txStatusOf(rm))
	}
	return out, nil
}

// txStatusOf unpacks a wire map into a typed TxStatus record.
func txStatusOf(m map[string]any) TxStatus {
	t, _ := m["terminal"].(bool)
	return TxStatus{
		TxID: mS(m, "tx_id"), State: mS(m, "state"), Reason: mS(m, "reason"),
		ConnID: mI(m, "conn_id"), Terminal: t,
		Stuck: time.Duration(mI(m, "stuck_ms")) * time.Millisecond,
	}
}

// IPAdmitted asks the daemon whether an address may be used by the
// authenticated user, and which allowlist layer admitted it.
//
// The address is supplied by the caller because the daemon cannot see it: a
// gateway reaches the daemon over loopback, so the peer the daemon observes
// is the gateway. The DECISION stays with the daemon, which is the only
// process holding the rules.
func (b *Bound) IPAdmitted(ctx context.Context, ip string) (bool, string, error) {
	res, err := b.authed(ctx, "auth.ip_admitted", ip)
	if err != nil {
		return false, "", err
	}
	m, _ := res.(map[string]any)
	admitted, _ := m["admitted"].(bool)
	return admitted, mS(m, "source"), nil
}

// ShutdownServer asks the connected server to drain and exit (admin
// only). The disconnect watcher then drives the reconnect, which spawns
// a fresh server when one is configured — that is the restart.
func (b *Bound) ShutdownServer(ctx context.Context) error {
	_, err := b.authed(ctx, "sys.shutdown")
	return err
}

// ConnInfo is one stored connection.
type ConnInfo struct {
	ID     int64
	Name   string
	Engine string
	// Profile is the capability profile: "v1compat" or "session".
	Profile string
	// FrontDoorExposed is the independent network-reachability decision.
	FrontDoorExposed bool
	// TargetDB is the database name inside the connection's DSN, when the
	// engine yields one. It is what a client types into a Database field, and
	// the fact whose absence cost an evening.
	TargetDB string
}

// Connections fetches all registered database connections.
func (b *Bound) Connections(ctx context.Context) ([]ConnInfo, error) {
	res, err := b.authed(ctx, "conn.list")
	if err != nil {
		return nil, err
	}
	var out []ConnInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, ConnInfo{
			ID: mI(m, "id"), Name: mS(m, "name"), Engine: mS(m, "engine"),
			Profile: mS(m, "profile"), FrontDoorExposed: mB(m, "frontdoor_exposed"),
			TargetDB: mS(m, "target_db"),
		})
	}
	return out, nil
}

// FrontDoorEndpoint is what a client needs to dial the front door, read from
// the LIVE listener rather than from configuration.
type FrontDoorEndpoint struct {
	Enabled    bool
	Listening  bool
	Addr       string
	HostNames  []string
	RootCAFile string
	// Cleartext reports that the live listener is serving WITHOUT TLS.
	Cleartext bool
}

// Configured reports whether a token minted now could actually be used.
//
// Enabled-but-not-listening is the case worth naming: the operator asked for
// the surface and it failed to start, so a credential minted against it works
// nowhere and nothing on screen would say so.
func (e FrontDoorEndpoint) Configured() bool { return e.Enabled && e.Listening }

// FrontDoorEndpoint asks the daemon where the front door actually is.
func (b *Bound) FrontDoorEndpoint(ctx context.Context) (FrontDoorEndpoint, error) {
	res, err := b.authed(ctx, "frontdoor.endpoint")
	if err != nil {
		return FrontDoorEndpoint{}, err
	}
	m, _ := res.(map[string]any)
	out := FrontDoorEndpoint{
		Enabled:    mB(m, "enabled"),
		Listening:  mB(m, "listening"),
		Addr:       mS(m, "addr"),
		RootCAFile: mS(m, "root_ca_file"),
		Cleartext:  mB(m, "cleartext"),
	}
	for _, h := range asList(m["host_names"]) {
		if s, ok := h.(string); ok {
			out.HostNames = append(out.HostNames, s)
		}
	}
	return out, nil
}

// KeyslotStatus is what the daemon reports about its unattended unlock
// (the locked-daemon contract).
//
// The daemon prints its banner ONCE, at start, to a terminal nobody may be
// watching. This is how an operator asks later — from the TUI, at the moment
// developers start being refused — why the store is locked.
type KeyslotStatus struct {
	// Attempted is false when no keyfile is configured: an install that never
	// asked for unattended unlock, which must not read as one that failed.
	Attempted bool
	// Unlocked reports whether the KEYSLOT opened the store this boot.
	Unlocked bool
	// Reason names the ground when it did not.
	Reason string
	// StoreUnlocked is the store's state NOW, which is a different question:
	// a failed keyslot followed by a passphrase login leaves the keyslot
	// failed and the store open, and an operator needs both answers.
	StoreUnlocked bool

	// --- what has been proven SINCE start, which is a THIRD question ---
	//
	// The fields above are the boot probe's findings and never change. These
	// are why: an operator who cut a slot from this very modal was still shown
	// the startup failure, read it as a silent no-op, and rebooted the machine
	// to find out whether it had worked. It had.
	Checked      bool   // anything proven since start?
	Verified     bool   // and did it open the store?
	VerifiedAt   string // when (RFC3339, empty if never)
	VerifyReason string // why not, if it did not
	SlotPresent  bool   // did a slot exist at that moment...
	// ...and could the store be asked at all. False means UNKNOWN, not
	// absent: a failed lookup used to render as a deliberate removal.
	SlotPresentKnown bool
}

// KeyslotStatus asks the daemon why it is (or is not) unlocked.
func (b *Bound) KeyslotStatus(ctx context.Context) (KeyslotStatus, error) {
	res, err := b.authed(ctx, "keyslot.status")
	if err != nil {
		return KeyslotStatus{}, err
	}
	m, _ := res.(map[string]any)
	return KeyslotStatus{
		Attempted:        mB(m, "attempted"),
		Unlocked:         mB(m, "unlocked"),
		Reason:           mS(m, "reason"),
		StoreUnlocked:    mB(m, "store_unlocked"),
		Checked:          mB(m, "checked"),
		Verified:         mB(m, "verified"),
		VerifiedAt:       mS(m, "verified_at"),
		VerifyReason:     mS(m, "verify_reason"),
		SlotPresent:      mB(m, "slot_present"),
		SlotPresentKnown: mB(m, "slot_present_known"),
	}, nil
}

// EnrollKeyslot cuts the service keyslot. Admin only, and only from an
// unlocked store — you cannot wrap a master key you do not hold.
func (b *Bound) EnrollKeyslot(ctx context.Context) error {
	_, err := b.authed(ctx, "keyslot.enroll")
	return err
}

// RemoveKeyslot deletes the slot AND its keyfile, so the next restart needs a
// passphrase again.
func (b *Bound) RemoveKeyslot(ctx context.Context) error {
	_, err := b.authed(ctx, "keyslot.remove")
	return err
}

// SetConnectionProfile switches a connection's capability profile. Admin only.
func (b *Bound) SetConnectionProfile(ctx context.Context, connID int64, profile string) error {
	_, err := b.authed(ctx, "conn.set_profile", connID, profile)
	return err
}

// SetConnectionExposure changes whether the front door may reach a connection.
// The server requires administrative authority.
func (b *Bound) SetConnectionExposure(ctx context.Context, connID int64, exposed bool) error {
	_, err := b.authed(ctx, "conn.set_exposure", connID, exposed)
	return err
}

// CreateConnection registers a new database connection with name, engine dialect, and DSN.
func (b *Bound) CreateConnection(ctx context.Context, name, engine, dsn string) (int64, error) {
	res, err := b.authed(ctx, "conn.create", name, engine, dsn)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

// TestConnection verifies connectivity to the database identified by connID.
func (b *Bound) TestConnection(ctx context.Context, connID int64) error {
	_, err := b.authed(ctx, "conn.test", connID)
	return err
}

// DeleteConnection removes the registered connection identified by connID.
func (b *Bound) DeleteConnection(ctx context.Context, connID int64) error {
	_, err := b.authed(ctx, "conn.delete", connID)
	return err
}

// WorkspaceInfo is one workspace view row.
type WorkspaceInfo struct {
	ID          int64
	Name        string
	Connections []ConnInfo
}

// Workspaces returns the list of all workspaces and their attached connections.
func (b *Bound) Workspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	res, err := b.authed(ctx, "workspace.list")
	if err != nil {
		return nil, err
	}
	var out []WorkspaceInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		w := WorkspaceInfo{ID: mI(m, "id"), Name: mS(m, "name")}
		for _, cr := range asList(m["connections"]) {
			cm, _ := cr.(map[string]any)
			w.Connections = append(w.Connections,
				ConnInfo{ID: mI(cm, "id"), Name: mS(cm, "name"), Engine: mS(cm, "engine")})
		}
		out = append(out, w)
	}
	return out, nil
}

// CreateWorkspace creates a new named workspace group.
func (b *Bound) CreateWorkspace(ctx context.Context, name string) (int64, error) {
	res, err := b.authed(ctx, "workspace.create", name)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

// RenameWorkspace renames an existing workspace.
func (b *Bound) RenameWorkspace(ctx context.Context, wsID int64, name string) error {
	_, err := b.authed(ctx, "workspace.rename", wsID, name)
	return err
}

// AttachConnection associates a database connection with a workspace.
func (b *Bound) AttachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := b.authed(ctx, "workspace.attach", wsID, connID)
	return err
}

// DetachConnection disassociates a database connection from a workspace.
func (b *Bound) DetachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := b.authed(ctx, "workspace.detach", wsID, connID)
	return err
}

// DeleteWorkspace deletes a workspace group.
func (b *Bound) DeleteWorkspace(ctx context.Context, wsID int64) error {
	_, err := b.authed(ctx, "workspace.delete", wsID)
	return err
}

// TableInfo is one explorer relation with its server-quoted identifier.
//
// Partitioned/IsPartition/Parent carry the Postgres partition role,
// zero-valued on other dialects and un-partitioned relations. Parent is a
// same-schema relation name only.
type TableInfo struct {
	Schema      string
	Name        string
	Kind        string
	Quoted      string
	Partitioned bool
	IsPartition bool
	Parent      string
}

// Schemas lists all schema names in the database for connID.
func (b *Bound) Schemas(ctx context.Context, connID int64) ([]string, error) {
	res, err := b.authed(ctx, "schema.schemas", connID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range asList(res) {
		if n, ok := v.(string); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Tables lists all relations (tables, views, partitions) in schema for connID.
func (b *Bound) Tables(ctx context.Context, connID int64, schema string) ([]TableInfo, error) {
	res, err := b.authed(ctx, "schema.tables", connID, schema)
	if err != nil {
		return nil, err
	}
	var out []TableInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, TableInfo{
			Schema: mS(m, "schema"), Name: mS(m, "name"),
			Kind: mS(m, "kind"), Quoted: mS(m, "quoted"),
			Partitioned: mB(m, "partitioned"), IsPartition: mB(m, "is_partition"),
			Parent: mS(m, "parent"),
		})
	}
	return out, nil
}

// ColumnInfo is one table column.
type ColumnInfo struct {
	Name     string
	Type     string
	Nullable bool
	PK       bool
}

// Columns lists all columns for table in schema for connID.
func (b *Bound) Columns(ctx context.Context, connID int64, schema, table string) ([]ColumnInfo, error) {
	res, err := b.authed(ctx, "schema.columns", connID, schema, table)
	if err != nil {
		return nil, err
	}
	var out []ColumnInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, ColumnInfo{
			Name: mS(m, "name"), Type: mS(m, "type"),
			Nullable: mB(m, "nullable"), PK: mB(m, "pk"),
		})
	}
	return out, nil
}

// RoutineInfo is one stored routine.
type RoutineInfo struct {
	Name      string
	Kind      string
	Signature string
}

// Routines lists stored procedures or functions in schema for connID, reporting dialect support.
func (b *Bound) Routines(ctx context.Context, connID int64, schema string) (supported bool, routines []RoutineInfo, err error) {
	res, err := b.authed(ctx, "schema.routines", connID, schema)
	if err != nil {
		return false, nil, err
	}
	m, _ := res.(map[string]any)
	supported = mB(m, "supported")
	for _, row := range asList(m["routines"]) {
		rm, _ := row.(map[string]any)
		routines = append(routines, RoutineInfo{
			Name: mS(rm, "name"), Kind: mS(rm, "kind"), Signature: mS(rm, "signature"),
		})
	}
	return supported, routines, nil
}

// ExecResult is one exec.run outcome.
type ExecResult struct {
	// Statements is how many ran (a script may hold several).
	Statements int64
	Verb       string
	Class      string
	Columns    []string
	Rows       [][]any
	More       bool
	Affected   int64
	Duration   time.Duration
}

// Run executes the buffer as a SCRIPT: one statement or many, run in
// order server-side, with the last statement's result coming back.
//
// Since protocol 5 the server also decides HOW to run it: a script
// containing a transaction boundary runs inside one transaction, so a
// `BEGIN; …; COMMIT;` in the editor applies all or nothing; a script
// without one keeps the old statement-by-statement behaviour. The
// distinction is the server's, which is why this call site is unchanged.
func (b *Bound) Run(ctx context.Context, connID int64, sql string) (*ExecResult, error) {
	res, err := b.authed(ctx, "exec.run_script", connID, sql)
	if err != nil {
		return nil, err
	}
	outer, _ := res.(map[string]any)
	statements := mI(outer, "statements")
	m, ok := outer["result"].(map[string]any)
	if !ok {
		// Every statement was a write with no rows to show.
		return &ExecResult{Verb: "OK", Statements: statements}, nil
	}
	out := &ExecResult{
		Statements: statements,
		Verb:       mS(m, "verb"), Class: mS(m, "class"),
		More: mB(m, "more"), Affected: mI(m, "affected"),
		Duration: time.Duration(mI(m, "duration_ms")) * time.Millisecond,
	}
	for _, c := range asList(m["columns"]) {
		if cs, ok := c.(string); ok {
			out.Columns = append(out.Columns, cs)
		}
	}
	for _, r := range asList(m["rows"]) {
		if rr, ok := r.([]any); ok {
			out.Rows = append(out.Rows, rr)
		}
	}
	return out, nil
}

// --- wire decode helpers -----------------------------------------------------

// asList safely unpacks a dynamic slice or returns nil.
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// mS retrieves a string value from a wire map by key.
func mS(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// mI retrieves an int64 value from a wire map by key.
func mI(m map[string]any, k string) int64 {
	n, _ := m[k].(int64)
	return n
}

// mSS decodes a msgpack array of strings. The wire hands back []any, so each
// element is asserted individually and a non-string is dropped rather than
// panicking a UI goroutine on a malformed reply.
func mSS(m map[string]any, k string) []string {
	raw, _ := m[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mB retrieves a boolean value from a wire map by key.
func mB(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// WireErrorMessage renders an error for the status bar / floats: the
// structured wire message when present, the plain error otherwise.
func WireErrorMessage(err error) string {
	var re *golibrpc.Error
	if errors.As(err, &re) {
		return re.Message
	}
	return err.Error()
}

// UserRow is one account row from auth.user_list (admin only).
type UserRow struct {
	ID       int64
	Name     string
	Role     string
	Disabled bool
}

// Users lists all registered user accounts (admin only).
func (b *Bound) Users(ctx context.Context) ([]UserRow, error) {
	res, err := b.authed(ctx, "auth.user_list")
	if err != nil {
		return nil, err
	}
	var out []UserRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, UserRow{
			ID: mI(m, "id"), Name: mS(m, "name"),
			Role: mS(m, "role"), Disabled: mB(m, "disabled"),
		})
	}
	return out, nil
}

// CreateUser registers a new user with initial credentials and access role.
func (b *Bound) CreateUser(ctx context.Context, name, pass, role string) (int64, error) {
	res, err := b.authed(ctx, "auth.user_create", name, pass, role)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

// SetUserRole modifies an existing user's authorization role.
func (b *Bound) SetUserRole(ctx context.Context, userID int64, role string) error {
	_, err := b.authed(ctx, "auth.user_role", userID, role)
	return err
}

// SetUserDisabled enables or disables a user account.
func (b *Bound) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	_, err := b.authed(ctx, "auth.user_disable", userID, disabled)
	return err
}

// RemoveUser permanently deletes a user account.
func (b *Bound) RemoveUser(ctx context.Context, userID int64) error {
	_, err := b.authed(ctx, "auth.user_remove", userID)
	return err
}

// ResetUserPassphrase updates a user's login passphrase.
func (b *Bound) ResetUserPassphrase(ctx context.Context, userID int64, newPass string) error {
	_, err := b.authed(ctx, "auth.passphrase_reset", userID, newPass)
	return err
}

// AddGrant awards connection access permissions to a user.
func (b *Bound) AddGrant(ctx context.Context, userID, connID int64, role string) error {
	_, err := b.authed(ctx, "auth.grant_add", userID, connID, role)
	return err
}

// AllowlistEntry is one global-allowlist line as the admin screen shows it.
// Config-seeded entries are read-only at runtime (Config true, ID 0);
// managed store rows carry their row id.
type AllowlistEntry struct {
	ID     int64
	CIDR   string
	Note   string
	Config bool
}

// Allowlist retrieves the global CIDR allowlist entries.
func (b *Bound) Allowlist(ctx context.Context) ([]AllowlistEntry, error) {
	res, err := b.authed(ctx, "auth.allowlist_list")
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	var out []AllowlistEntry
	for _, c := range asList(m["config"]) {
		if cs, ok := c.(string); ok {
			out = append(out, AllowlistEntry{CIDR: cs, Note: "(config — read-only)", Config: true})
		}
	}
	for _, row := range asList(m["rows"]) {
		rm, _ := row.(map[string]any)
		out = append(out, AllowlistEntry{ID: mI(rm, "id"), CIDR: mS(rm, "cidr"), Note: mS(rm, "note")})
	}
	return out, nil
}

// AddAllowedIP appends a CIDR block and note to the global allowlist.
func (b *Bound) AddAllowedIP(ctx context.Context, cidr, note string) error {
	_, err := b.authed(ctx, "auth.allowlist_add", cidr, note)
	return err
}

// RemoveAllowedIP deletes a CIDR block from the global allowlist.
func (b *Bound) RemoveAllowedIP(ctx context.Context, cidr string) error {
	_, err := b.authed(ctx, "auth.allowlist_remove", cidr)
	return err
}

// UserIPRow is one per-user allowlist row.
type UserIPRow struct {
	ID     int64
	UserID int64
	CIDR   string
	Label  string
}

// UserIPs retrieves the per-user IP allowlist for userID.
func (b *Bound) UserIPs(ctx context.Context, userID int64) ([]UserIPRow, error) {
	res, err := b.authed(ctx, "auth.user_allowlist_list", userID)
	if err != nil {
		return nil, err
	}
	var out []UserIPRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, UserIPRow{
			ID: mI(m, "id"), UserID: mI(m, "user_id"),
			CIDR: mS(m, "cidr"), Label: mS(m, "label"),
		})
	}
	return out, nil
}

// AddUserIP adds a CIDR (or bare address) to userID's allowlist. An empty
// cidr asks the server to use the address this session connects from.
func (b *Bound) AddUserIP(ctx context.Context, userID int64, cidr, label string) error {
	_, err := b.authed(ctx, "auth.user_allowlist_add", userID, cidr, label)
	return err
}

// RemoveUserIP deletes an address entry from userID's allowlist.
func (b *Bound) RemoveUserIP(ctx context.Context, userID, rowID int64) error {
	_, err := b.authed(ctx, "auth.user_allowlist_remove", userID, rowID)
	return err
}

// PATRow is one personal access token as the server is willing to describe
// it. There is deliberately no secret, digest or selector here: the server
// publishes none of them after creation, and a row that carried the selector
// would be half a credential.
type PATRow struct {
	Name       string
	CreatedAt  string
	ExpiresAt  string
	LastUsed   string
	Revoked    bool
	AllowedIPs []string
}

// PATSecret is what creating a token returns. Secret exists here and nowhere
// else, ever again — the store keeps a SHA-256, so if this value is not shown
// to the user now it is not recoverable by anyone.
type PATSecret struct {
	Name      string
	Secret    string
	ExpiresAt string
}

// PATs lists userID's tokens. Pass the caller's own id for self-service.
func (b *Bound) PATs(ctx context.Context, userID int64) ([]PATRow, error) {
	res, err := b.authed(ctx, "auth.token_list", userID)
	if err != nil {
		return nil, err
	}
	var out []PATRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, patRowFromWire(m))
	}
	return out, nil
}

// patRowFromWire decodes one auth.token_list row.
//
// `allowed_ips` is a comma-separated STRING on the wire — meta.PAT.AllowedIPs
// is a string, canonicalized on write — not a list. Decoding it as a list
// yielded nil for every restricted token, and the manager then labelled it
// "any": the UI told the operator a restricted token carried no restriction.
// Split out so a test can cross the real wire shape, which is exactly what
// the helper-level tests could not reach.
func patRowFromWire(m map[string]any) PATRow {
	return PATRow{
		Name:       mS(m, "name"),
		CreatedAt:  mS(m, "created_at"),
		ExpiresAt:  mS(m, "expires_at"),
		LastUsed:   mS(m, "last_used"),
		Revoked:    mB(m, "revoked"),
		AllowedIPs: splitAllowedIPs(mS(m, "allowed_ips")),
	}
}

// splitAllowedIPs parses the CSV, dropping blanks so " , " does not become a
// phantom restriction. Empty yields nil, which means the token INHERITS the
// user's admission set rather than reaching nowhere.
func splitAllowedIPs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CreatePAT mints a token for the CALLING user, BOUND TO ONE CONNECTION.
//
// days is 0 for the server default or 1..365; allowedIPs must be a subset of
// the caller's own allowlist rows and is sent as the CSV the wire expects.
//
// connID is not optional: every PAT names exactly one
// connection, and the server refuses a mint against one the caller has no
// grant on, one that is not enabled for front-door use, or one whose target
// database name has not been recorded.
// CreatePAT mints a token. approvedToAdd is the exact canonical set the
// operator was SHOWN and approved for addition to their own allowlist; nil
// means nothing was approved, and the daemon keeps its subset refusal.
//
// The second return is a STALE-APPROVAL set: non-nil means NOTHING was
// created, because the rows that would now be added are no longer the rows
// that were approved, and it carries the new exact set to re-confirm. It is a
// separate return rather than an error because the caller must re-prompt, not
// report a fault.
func (b *Bound) CreatePAT(ctx context.Context, name string, days int64, allowedIPs []string, connID int64, debugCleartext bool, approvedToAdd []string) (PATSecret, []string, error) {
	flag := int64(0)
	if debugCleartext {
		flag = 1
	}
	res, err := b.authed(ctx, "auth.token_create", name, days, strings.Join(allowedIPs, ","),
		connID, flag, strings.Join(approvedToAdd, ","))
	if err != nil {
		return PATSecret{}, nil, err
	}
	m, _ := res.(map[string]any)
	if mB(m, "stale_approval") {
		return PATSecret{}, mSS(m, "missing"), nil
	}
	return PATSecret{
		Name:      mS(m, "name"),
		Secret:    mS(m, "secret"),
		ExpiresAt: mS(m, "expires_at"),
	}, nil, nil
}

// PATAllowlistPreview reports which CIDRs minting with these restrictions
// would ADD to the caller's own allowlist. Empty means the operation does not
// widen, and no confirmation is needed.
//
// Presentation only: the daemon recomputes this under the owner's lock and may
// add nothing that is not in the set the operator then approved.
func (b *Bound) PATAllowlistPreview(ctx context.Context, allowedIPs []string) ([]string, error) {
	res, err := b.authed(ctx, "auth.token_allowlist_preview", strings.Join(allowedIPs, ","))
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	return mSS(m, "missing"), nil
}

// CAPem is the front door's CA certificate, as text.
type CAPem struct {
	// Path is where it lives on the DAEMON's host, shown for reference only:
	// a developer running the TUI over a tunnel cannot read it, which is why
	// this carries the contents.
	Path string
	// PEM is the certificate itself, empty when the install uses system roots.
	PEM string
	// SystemRoots distinguishes "no private CA" from "unreadable", so an empty
	// document does not have to be guessed at.
	SystemRoots bool
}

// FrontDoorCAPem fetches the CA certificate a client must trust.
//
// The CONTENTS, not the path: the path is useless to the person who needs it.
// A developer running the TUI over a tunnel cannot read a file on the daemon's
// host, and on the host itself /etc/autodb/tls is 0710, so only root and the
// service account can traverse it.
func (b *Bound) FrontDoorCAPem(ctx context.Context) (CAPem, error) {
	res, err := b.authed(ctx, "frontdoor.ca_pem")
	if err != nil {
		return CAPem{}, err
	}
	m, _ := res.(map[string]any)
	return CAPem{
		Path:        mS(m, "path"),
		PEM:         mS(m, "pem"),
		SystemRoots: mB(m, "system_roots"),
	}, nil
}

// RevokePAT revokes a personal access token by name for userID.
func (b *Bound) RevokePAT(ctx context.Context, userID int64, name string) error {
	_, err := b.authed(ctx, "auth.token_revoke", userID, name)
	return err
}
