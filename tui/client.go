package tui

import (
	"context"
	"errors"
	"fmt"
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

	mu       sync.Mutex
	client   *golibrpc.Client
	instance string
	version  string
	token    string
	user     UserInfo
	gen      uint64 // state epoch; bumps when a (re)connect/disconnect BEGINS
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
	if log == nil {
		log = logger.Nop{}
	}
	return &Session{addr: addr, log: log, spawn: spawn}
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

// snapshot captures the state a worker-goroutine operation runs against.
func (s *Session) snapshot() (cli *golibrpc.Client, gen uint64, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client, s.gen, s.token
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
		cli, err = golibrpc.Dial(ctx, s.addr, msgpackrpc.New(nil))
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
		return false, fmt.Errorf("handshake: %w", err)
	}
	m, _ := res.(map[string]any)
	inst, _ := m["instance"].(string)
	ver, _ := m["version"].(string)

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

// Close shuts the underlying client down.
func (s *Session) Close() { s.Disconnect() }

// call issues a tokenless method (hello aside, only auth.needs_bootstrap).
func (s *Session) call(ctx context.Context, method string, params ...any) (any, error) {
	cli, _, _ := s.snapshot()
	if cli == nil {
		return nil, errors.New("tui: not connected")
	}
	return cli.Call(ctx, method, params...)
}

// authed is the token-first chokepoint: it snapshots the epoch and token,
// prepends the token, and on a public CodeAuth failure invalidates the
// login state — but ONLY if this exact token is still current in this
// exact epoch, so a stale failure can never clear a newer login.
func (s *Session) authed(ctx context.Context, method string, extra ...any) (any, error) {
	cli, gen, tok := s.snapshot()
	if cli == nil {
		return nil, errors.New("tui: not connected")
	}
	params := append([]any{tok}, extra...)
	res, err := cli.Call(ctx, method, params...)
	if err != nil {
		var re *golibrpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeAuth {
			// Stale token or locked store: the UI's one recovery for both
			// is the login flow (the Model watches the token-empty edge).
			s.mu.Lock()
			if s.gen == gen && s.token == tok {
				s.token = ""
				s.user = UserInfo{}
			}
			s.mu.Unlock()
		}
		return nil, err
	}
	return res, nil
}

// --- typed projections -----------------------------------------------------

func (s *Session) NeedsBootstrap(ctx context.Context) (bool, error) {
	res, err := s.call(ctx, "auth.needs_bootstrap")
	if err != nil {
		return false, err
	}
	b, _ := res.(bool)
	return b, nil
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

func (s *Session) Bootstrap(ctx context.Context, name, pass string) error {
	cli, gen, _ := s.snapshot()
	if cli == nil {
		return errors.New("tui: not connected")
	}
	res, err := cli.Call(ctx, "auth.bootstrap", name, pass)
	if err != nil {
		return err
	}
	s.adoptLogin(res, gen)
	return nil
}

func (s *Session) Login(ctx context.Context, name, pass string) error {
	cli, gen, _ := s.snapshot()
	if cli == nil {
		return errors.New("tui: not connected")
	}
	res, err := cli.Call(ctx, "auth.login", name, pass)
	if err != nil {
		return err
	}
	s.adoptLogin(res, gen)
	return nil
}

func (s *Session) Logout(ctx context.Context) error {
	cli, gen, tok := s.snapshot()
	if tok == "" || cli == nil {
		return nil
	}
	_, err := cli.Call(ctx, "auth.logout", tok)
	s.mu.Lock()
	if s.gen == gen && s.token == tok {
		s.token = ""
		s.user = UserInfo{}
	}
	s.mu.Unlock()
	return err
}

// ConnInfo is one stored connection.
type ConnInfo struct {
	ID     int64
	Name   string
	Engine string
}

func (s *Session) Connections(ctx context.Context) ([]ConnInfo, error) {
	res, err := s.authed(ctx, "conn.list")
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

func (s *Session) CreateConnection(ctx context.Context, name, engine, dsn string) (int64, error) {
	res, err := s.authed(ctx, "conn.create", name, engine, dsn)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) TestConnection(ctx context.Context, connID int64) error {
	_, err := s.authed(ctx, "conn.test", connID)
	return err
}

func (s *Session) DeleteConnection(ctx context.Context, connID int64) error {
	_, err := s.authed(ctx, "conn.delete", connID)
	return err
}

// WorkspaceInfo is one workspace view row.
type WorkspaceInfo struct {
	ID          int64
	Name        string
	Connections []ConnInfo
}

func (s *Session) Workspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	res, err := s.authed(ctx, "workspace.list")
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

func (s *Session) CreateWorkspace(ctx context.Context, name string) (int64, error) {
	res, err := s.authed(ctx, "workspace.create", name)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) RenameWorkspace(ctx context.Context, wsID int64, name string) error {
	_, err := s.authed(ctx, "workspace.rename", wsID, name)
	return err
}

func (s *Session) AttachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := s.authed(ctx, "workspace.attach", wsID, connID)
	return err
}

func (s *Session) DetachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := s.authed(ctx, "workspace.detach", wsID, connID)
	return err
}

func (s *Session) DeleteWorkspace(ctx context.Context, wsID int64) error {
	_, err := s.authed(ctx, "workspace.delete", wsID)
	return err
}

// TableInfo is one explorer relation with its server-quoted identifier.
type TableInfo struct {
	Schema string
	Name   string
	Kind   string
	Quoted string
}

func (s *Session) Schemas(ctx context.Context, connID int64) ([]string, error) {
	res, err := s.authed(ctx, "schema.schemas", connID)
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

func (s *Session) Tables(ctx context.Context, connID int64, schema string) ([]TableInfo, error) {
	res, err := s.authed(ctx, "schema.tables", connID, schema)
	if err != nil {
		return nil, err
	}
	var out []TableInfo
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, TableInfo{
			Schema: mS(m, "schema"), Name: mS(m, "name"),
			Kind: mS(m, "kind"), Quoted: mS(m, "quoted"),
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

func (s *Session) Columns(ctx context.Context, connID int64, schema, table string) ([]ColumnInfo, error) {
	res, err := s.authed(ctx, "schema.columns", connID, schema, table)
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

func (s *Session) Routines(ctx context.Context, connID int64, schema string) (supported bool, routines []RoutineInfo, err error) {
	res, err := s.authed(ctx, "schema.routines", connID, schema)
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
	Verb     string
	Class    string
	Columns  []string
	Rows     [][]any
	More     bool
	Affected int64
	Duration time.Duration
}

func (s *Session) Run(ctx context.Context, connID int64, sql string) (*ExecResult, error) {
	res, err := s.authed(ctx, "exec.run", connID, sql)
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	out := &ExecResult{
		Verb: mS(m, "verb"), Class: mS(m, "class"),
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

func (s *Session) Users(ctx context.Context) ([]UserRow, error) {
	res, err := s.authed(ctx, "auth.user_list")
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

func (s *Session) CreateUser(ctx context.Context, name, pass, role string) (int64, error) {
	res, err := s.authed(ctx, "auth.user_create", name, pass, role)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) SetUserRole(ctx context.Context, userID int64, role string) error {
	_, err := s.authed(ctx, "auth.user_role", userID, role)
	return err
}

func (s *Session) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	_, err := s.authed(ctx, "auth.user_disable", userID, disabled)
	return err
}

func (s *Session) RemoveUser(ctx context.Context, userID int64) error {
	_, err := s.authed(ctx, "auth.user_remove", userID)
	return err
}

func (s *Session) ResetUserPassphrase(ctx context.Context, userID int64, newPass string) error {
	_, err := s.authed(ctx, "auth.passphrase_reset", userID, newPass)
	return err
}

func (s *Session) AddGrant(ctx context.Context, userID, connID int64, role string) error {
	_, err := s.authed(ctx, "auth.grant_add", userID, connID, role)
	return err
}
