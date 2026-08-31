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

// Session is the TUI's ONLY path to the core (ADR-0057 §7 — the client
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
// failure before giving up (ADR-0056 §3 — a spawned server that exits
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
}

// Bind pins the current epoch. Call it where the user's intent forms —
// the action issuance point — not inside the worker.
func (s *Session) Bind() *Bound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Bound{s: s, cli: s.client, gen: s.gen, token: s.token}
}

// Gen reports the pinned epoch (result tagging at issuance sites).
func (b *Bound) Gen() uint64 { return b.gen }

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

// Connect implements the FE contract (ADR-0056 §3): dial; on refusal spawn
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
		// cached assumption is stale (ADR-0057 §7).
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

func (b *Bound) NeedsBootstrap(ctx context.Context) (bool, error) {
	res, err := b.call(ctx, "auth.needs_bootstrap")
	if err != nil {
		return false, err
	}
	needs, _ := res.(bool)
	return needs, nil
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

func (b *Bound) Bootstrap(ctx context.Context, name, pass string) error {
	res, err := b.call(ctx, "auth.bootstrap", name, pass)
	if err != nil {
		return err
	}
	b.s.adoptLogin(res, b.gen)
	return nil
}

func (b *Bound) Login(ctx context.Context, name, pass string) error {
	res, err := b.call(ctx, "auth.login", name, pass)
	if err != nil {
		return err
	}
	b.s.adoptLogin(res, b.gen)
	return nil
}

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
}

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
			Error: mS(m, "error"),
		})
	}
	return out, nil
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
}

func (b *Bound) Connections(ctx context.Context) ([]ConnInfo, error) {
	res, err := b.authed(ctx, "conn.list")
	if err != nil {
		return nil, err
	}
	var out []ConnInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, ConnInfo{ID: mI(m, "id"), Name: mS(m, "name"), Engine: mS(m, "engine")})
	}
	return out, nil
}

func (b *Bound) CreateConnection(ctx context.Context, name, engine, dsn string) (int64, error) {
	res, err := b.authed(ctx, "conn.create", name, engine, dsn)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (b *Bound) TestConnection(ctx context.Context, connID int64) error {
	_, err := b.authed(ctx, "conn.test", connID)
	return err
}

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

func (b *Bound) CreateWorkspace(ctx context.Context, name string) (int64, error) {
	res, err := b.authed(ctx, "workspace.create", name)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (b *Bound) RenameWorkspace(ctx context.Context, wsID int64, name string) error {
	_, err := b.authed(ctx, "workspace.rename", wsID, name)
	return err
}

func (b *Bound) AttachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := b.authed(ctx, "workspace.attach", wsID, connID)
	return err
}

func (b *Bound) DetachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := b.authed(ctx, "workspace.detach", wsID, connID)
	return err
}

func (b *Bound) DeleteWorkspace(ctx context.Context, wsID int64) error {
	_, err := b.authed(ctx, "workspace.delete", wsID)
	return err
}

// TableInfo is one explorer relation with its server-quoted identifier.
//
// Partitioned/IsPartition/Parent carry the Postgres partition role (ADR-0077),
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

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func mS(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mI(m map[string]any, k string) int64 {
	n, _ := m[k].(int64)
	return n
}

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

func (b *Bound) CreateUser(ctx context.Context, name, pass, role string) (int64, error) {
	res, err := b.authed(ctx, "auth.user_create", name, pass, role)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (b *Bound) SetUserRole(ctx context.Context, userID int64, role string) error {
	_, err := b.authed(ctx, "auth.user_role", userID, role)
	return err
}

func (b *Bound) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	_, err := b.authed(ctx, "auth.user_disable", userID, disabled)
	return err
}

func (b *Bound) RemoveUser(ctx context.Context, userID int64) error {
	_, err := b.authed(ctx, "auth.user_remove", userID)
	return err
}

func (b *Bound) ResetUserPassphrase(ctx context.Context, userID int64, newPass string) error {
	_, err := b.authed(ctx, "auth.passphrase_reset", userID, newPass)
	return err
}

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

func (b *Bound) AddAllowedIP(ctx context.Context, cidr, note string) error {
	_, err := b.authed(ctx, "auth.allowlist_add", cidr, note)
	return err
}

func (b *Bound) RemoveAllowedIP(ctx context.Context, cidr string) error {
	_, err := b.authed(ctx, "auth.allowlist_remove", cidr)
	return err
}

// UserIPRow is one per-user allowlist row (ADR-0075 §4 second layer).
type UserIPRow struct {
	ID     int64
	UserID int64
	CIDR   string
	Label  string
}

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

func (b *Bound) RemoveUserIP(ctx context.Context, userID, rowID int64) error {
	_, err := b.authed(ctx, "auth.user_allowlist_remove", userID, rowID)
	return err
}
