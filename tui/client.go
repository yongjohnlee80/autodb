package tui

import (
	"context"
	"errors"
	"fmt"
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
type Session struct {
	addr  string
	log   logger.Logger
	spawn func() error // how to start `autodb --serve`; nil = never spawn

	client   *golibrpc.Client
	instance string
	version  string

	token string
	user  UserInfo
	gen   uint64 // connection generation; bumps on every (re)connect
}

// UserInfo is the logged-in identity as reported by the server.
type UserInfo struct {
	ID   int64
	Name string
	Role string
}

// NewSession prepares an unconnected session.
func NewSession(addr string, log logger.Logger, spawn func() error) *Session {
	if log == nil {
		log = logger.Nop{}
	}
	return &Session{addr: addr, log: log, spawn: spawn}
}

// Gen reports the connection generation (stale-task filtering in the UI).
func (s *Session) Gen() uint64 { return s.gen }

// Token reports the bearer token ("" = not logged in).
func (s *Session) Token() string { return s.token }

// User reports the logged-in identity.
func (s *Session) User() UserInfo { return s.user }

// IsAdmin reports whether the logged-in user is an admin.
func (s *Session) IsAdmin() bool { return s.user.Role == "admin" }

// ServerVersion reports the connected server's version string.
func (s *Session) ServerVersion() string { return s.version }

// Done exposes the underlying client's terminal signal (nil-safe: an
// unconnected session returns a nil channel, which never fires).
func (s *Session) Done() <-chan struct{} {
	if s.client == nil {
		return nil
	}
	return s.client.Done()
}

// Err reports the terminal cause after Done fires.
func (s *Session) Err() error {
	if s.client == nil {
		return nil
	}
	return s.client.Err()
}

// Connect implements the FE contract (ADR-0056 §3): dial; on refusal spawn
// `--serve` (when a spawner is configured) and retry with backoff; then
// hello at the current Protocol. It reports whether the server INSTANCE
// changed versus the previous connection — the caller drops cached state
// and re-prompts login on a change.
func (s *Session) Connect(ctx context.Context) (instanceChanged bool, err error) {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
	var cli *golibrpc.Client
	backoff := 100 * time.Millisecond
	spawned := false
	for {
		cli, err = golibrpc.Dial(ctx, s.addr, msgpackrpc.New(nil))
		if err == nil {
			break
		}
		if s.spawn != nil && !spawned {
			s.log.Log(logger.SeverityInfo, map[string]any{
				"tui": "session", "event": "spawning server", "addr": s.addr,
			})
			if serr := s.spawn(); serr != nil {
				return false, fmt.Errorf("spawn autodb --serve: %w", serr)
			}
			spawned = true
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
	s.gen++
	return instanceChanged, nil
}

// Close shuts the underlying client down.
func (s *Session) Close() {
	if s.client != nil {
		_ = s.client.Close()
	}
}

// call is the typed-call chokepoint. A public ErrLocked-shaped auth error
// invalidates the login state so the UI re-prompts (login also unlocks).
func (s *Session) call(ctx context.Context, method string, params ...any) (any, error) {
	if s.client == nil {
		return nil, errors.New("tui: not connected")
	}
	res, err := s.client.Call(ctx, method, params...)
	if err != nil {
		var re *golibrpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeAuth {
			// Stale token, locked store, or bad credentials: the UI's one
			// recovery for all three is the login flow.
			if method != "auth.login" && method != "auth.bootstrap" {
				s.token = ""
			}
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

func (s *Session) adoptLogin(res any) {
	m, _ := res.(map[string]any)
	s.token, _ = m["token"].(string)
	if u, ok := m["user"].(map[string]any); ok {
		s.user = UserInfo{ID: mI(u, "id"), Name: mS(u, "name"), Role: mS(u, "role")}
	}
}

func (s *Session) Bootstrap(ctx context.Context, name, pass string) error {
	res, err := s.call(ctx, "auth.bootstrap", name, pass)
	if err != nil {
		return err
	}
	s.adoptLogin(res)
	return nil
}

func (s *Session) Login(ctx context.Context, name, pass string) error {
	res, err := s.call(ctx, "auth.login", name, pass)
	if err != nil {
		return err
	}
	s.adoptLogin(res)
	return nil
}

func (s *Session) Logout(ctx context.Context) error {
	if s.token == "" {
		return nil
	}
	_, err := s.call(ctx, "auth.logout", s.token)
	s.token = ""
	s.user = UserInfo{}
	return err
}

// ConnInfo is one stored connection.
type ConnInfo struct {
	ID     int64
	Name   string
	Engine string
}

func (s *Session) Connections(ctx context.Context) ([]ConnInfo, error) {
	res, err := s.call(ctx, "conn.list", s.token)
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
	res, err := s.call(ctx, "conn.create", s.token, name, engine, dsn)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) TestConnection(ctx context.Context, connID int64) error {
	_, err := s.call(ctx, "conn.test", s.token, connID)
	return err
}

func (s *Session) DeleteConnection(ctx context.Context, connID int64) error {
	_, err := s.call(ctx, "conn.delete", s.token, connID)
	return err
}

// WorkspaceInfo is one workspace view row.
type WorkspaceInfo struct {
	ID          int64
	Name        string
	Connections []ConnInfo
}

func (s *Session) Workspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	res, err := s.call(ctx, "workspace.list", s.token)
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
	res, err := s.call(ctx, "workspace.create", s.token, name)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) AttachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := s.call(ctx, "workspace.attach", s.token, wsID, connID)
	return err
}

func (s *Session) DetachConnection(ctx context.Context, wsID, connID int64) error {
	_, err := s.call(ctx, "workspace.detach", s.token, wsID, connID)
	return err
}

func (s *Session) DeleteWorkspace(ctx context.Context, wsID int64) error {
	_, err := s.call(ctx, "workspace.delete", s.token, wsID)
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
	res, err := s.call(ctx, "schema.schemas", s.token, connID)
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
	res, err := s.call(ctx, "schema.tables", s.token, connID, schema)
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
	res, err := s.call(ctx, "schema.columns", s.token, connID, schema, table)
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
	res, err := s.call(ctx, "schema.routines", s.token, connID, schema)
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
	res, err := s.call(ctx, "exec.run", s.token, connID, sql)
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
	res, err := s.call(ctx, "auth.user_list", s.token)
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
	res, err := s.call(ctx, "auth.user_create", s.token, name, pass, role)
	if err != nil {
		return 0, err
	}
	id, _ := res.(int64)
	return id, nil
}

func (s *Session) SetUserRole(ctx context.Context, userID int64, role string) error {
	_, err := s.call(ctx, "auth.user_role", s.token, userID, role)
	return err
}

func (s *Session) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	_, err := s.call(ctx, "auth.user_disable", s.token, userID, disabled)
	return err
}

func (s *Session) RemoveUser(ctx context.Context, userID int64) error {
	_, err := s.call(ctx, "auth.user_remove", s.token, userID)
	return err
}

func (s *Session) AddGrant(ctx context.Context, userID, connID int64, role string) error {
	_, err := s.call(ctx, "auth.grant_add", s.token, userID, connID, role)
	return err
}
