package meta

import (
	"github.com/yongjohnlee80/golib/dao"
)

// Sort is the shared (currently empty) sort-key enum for meta-store schemas.
type Sort string

// Role values for users and grants. Enforcement semantics land in M3; the
// column CHECK constraints already pin the vocabulary.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleReader = "reader"
)

// sortableSchema is schema with ORDER BY keys registered.
//
// Paging needs a stable order, and the DAO will only order by keys the schema
// declares — so a table that is paged has to say how. Without it a LIMIT
// returns some rows rather than the first ones, and a repeated pass can read
// the same rows forever while others are never reached.
func sortableSchema[R any, C ~string](conn dao.DataConn, table string, id C,
	sorts map[Sort]string, fields map[C]dao.Field[R]) *dao.Schema[R, C, Sort, int64] {
	return dao.New(conn,
		dao.Table[R, C, Sort, int64](table),
		dao.ID[R, C, Sort, int64](id),
		dao.SortMap[R, C, Sort, int64](sorts),
		dao.Fields[R, C, Sort, int64](fields),
	)
}

// schema builds a meta-store entity schema: single table, int64 id, no joins.
func schema[R any, C ~string](conn dao.DataConn, table string, id C, fields map[C]dao.Field[R]) *dao.Schema[R, C, Sort, int64] {
	return dao.New(conn,
		dao.Table[R, C, Sort, int64](table),
		dao.ID[R, C, Sort, int64](id),
		dao.Fields[R, C, Sort, int64](fields),
	)
}

// --- users -------------------------------------------------------------------

// User is an autodb account (Objectives 12-13; auth semantics in ADR-0054).
// PassHash holds the PHC-encoded argon2id record; MKWrapped is this user's
// master-key keyslot (nonce-prefixed AES-GCM wrap).
type User struct {
	ID        int64
	Name      string
	Role      string
	PassHash  []byte
	MKWrapped []byte
	Disabled  int64 // 0/1 flag
	CreatedAt int64 // unix seconds
	UpdatedAt int64
}

type UserField string

const (
	UserID        UserField = "id"
	UserName      UserField = "name"
	UserRole      UserField = "role"
	UserPassHash  UserField = "pass_hash"
	UserMKWrapped UserField = "mk_wrapped"
	UserDisabled  UserField = "disabled"
	UserCreatedAt UserField = "created_at"
	UserUpdatedAt UserField = "updated_at"
)

func newUsers(conn dao.DataConn) *dao.Schema[*User, UserField, Sort, int64] {
	return schema(conn, "users", UserID, map[UserField]dao.Field[*User]{
		UserID:        {Column: "id", Scan: func(r *User) any { return &r.ID }},
		UserName:      {Column: "name", Scan: func(r *User) any { return &r.Name }, Value: func(r *User) any { return r.Name }},
		UserRole:      {Column: "role", Scan: func(r *User) any { return &r.Role }, Value: func(r *User) any { return r.Role }},
		UserPassHash:  {Column: "pass_hash", Scan: func(r *User) any { return &r.PassHash }, Value: func(r *User) any { return r.PassHash }},
		UserMKWrapped: {Column: "mk_wrapped", Scan: func(r *User) any { return &r.MKWrapped }, Value: func(r *User) any { return r.MKWrapped }},
		UserDisabled:  {Column: "disabled", Scan: func(r *User) any { return &r.Disabled }, Value: func(r *User) any { return r.Disabled }},
		UserCreatedAt: {Column: "created_at", Scan: func(r *User) any { return &r.CreatedAt }, Value: func(r *User) any { return r.CreatedAt }},
		UserUpdatedAt: {Column: "updated_at", Scan: func(r *User) any { return &r.UpdatedAt }, Value: func(r *User) any { return r.UpdatedAt }},
	})
}

// --- connections ---------------------------------------------------------------

// Connection is a managed target-database connection. DSNEnc is the
// encrypted-at-rest DSN (Objective 11; envelope shape decided in M3).
type Connection struct {
	ID     int64
	Name   string
	Engine string
	DSNEnc []byte
	// Profile is the connection's capability profile (ADR-0074 §2):
	// "v1compat" or "session". Existing rows read v1compat, so enabling
	// sessions is a per-connection decision rather than something a schema
	// upgrade did on your behalf.
	Profile string
	// Debug marks a connection used for debugging against a live target
	// (Amendment 2 C2): it takes the longer idle-in-transaction bound,
	// because a developer paused at a breakpoint inside a transaction should
	// not be rolled back mid-step. 0/1, matching users.disabled.
	Debug int64
	// PoolMaxConns is this connection's own bound on pooled connections
	// (ADR-0074 §1a). 0 takes the install-wide value; a larger number is
	// capped to it when the pool is opened, so a row cannot raise its own
	// share of a production database's connection budget.
	PoolMaxConns int64
	CreatedBy    int64
	CreatedAt    int64
	UpdatedAt    int64
	// TargetDB is the database name inside this connection's DSN, kept in
	// PLAINTEXT (ADR-0086 §3).
	//
	// The database NAME is not a secret — the DSN's credentials are — and a
	// column makes the startup `database` cross-check an indexed read instead
	// of N DSN decryptions on the auth path. It also lets the TUI show what a
	// connection actually points at, which is the fact whose absence cost an
	// evening.
	//
	// Derived with the driver's own parser, never by substring matching, and
	// populated wherever the DSN is set while the store is unlocked. A
	// session-profile connection is required to have one, which is what makes
	// the cross-check total rather than conditional. It is a CACHE of a fact
	// sealed inside the DSN, so it is VERIFIED against the decrypted DSN when a
	// session pins its target rather than trusted.
	TargetDB string
}

// IsDebug reports whether the connection carries the debug profile.
func (c *Connection) IsDebug() bool { return c.Debug != 0 }

// Connection capability profiles — the permitted values of connections.profile.
//
// They live HERE, in the package that owns the column, because both layers
// above need them and neither may import the other: core/auth gates PAT
// minting on a connection's profile (ADR-0086 §6) and core/exec decides
// statement admission from it, while core/auth sits BELOW core/exec and cannot
// reach exec.Profile. Defining the literal a second time in auth would make
// three copies of one fact — the migration DDL's default being the first.
// exec.Profile is derived from these rather than restating them.
const (
	ProfileV1Compat = "v1compat"
	ProfileSession  = "session"
)

type ConnField string

const (
	ConnID           ConnField = "id"
	ConnName         ConnField = "name"
	ConnEngine       ConnField = "engine"
	ConnDSNEnc       ConnField = "dsn_enc"
	ConnProfile      ConnField = "profile"
	ConnDebug        ConnField = "debug"
	ConnPoolMaxConns ConnField = "pool_max_conns"
	ConnCreatedBy    ConnField = "created_by"
	ConnCreatedAt    ConnField = "created_at"
	ConnUpdatedAt    ConnField = "updated_at"
	ConnTargetDB     ConnField = "target_db"
)

func newConnections(conn dao.DataConn) *dao.Schema[*Connection, ConnField, Sort, int64] {
	return schema(conn, "connections", ConnID, map[ConnField]dao.Field[*Connection]{
		ConnID:           {Column: "id", Scan: func(r *Connection) any { return &r.ID }},
		ConnName:         {Column: "name", Scan: func(r *Connection) any { return &r.Name }, Value: func(r *Connection) any { return r.Name }},
		ConnEngine:       {Column: "engine", Scan: func(r *Connection) any { return &r.Engine }, Value: func(r *Connection) any { return r.Engine }},
		ConnProfile:      {Column: "profile", Scan: func(r *Connection) any { return &r.Profile }, Value: func(r *Connection) any { return r.Profile }},
		ConnDebug:        {Column: "debug", Scan: func(r *Connection) any { return &r.Debug }, Value: func(r *Connection) any { return r.Debug }},
		ConnPoolMaxConns: {Column: "pool_max_conns", Scan: func(r *Connection) any { return &r.PoolMaxConns }, Value: func(r *Connection) any { return r.PoolMaxConns }},
		ConnDSNEnc:       {Column: "dsn_enc", Scan: func(r *Connection) any { return &r.DSNEnc }, Value: func(r *Connection) any { return r.DSNEnc }},
		ConnCreatedBy:    {Column: "created_by", Scan: func(r *Connection) any { return &r.CreatedBy }, Value: func(r *Connection) any { return r.CreatedBy }},
		ConnCreatedAt:    {Column: "created_at", Scan: func(r *Connection) any { return &r.CreatedAt }, Value: func(r *Connection) any { return r.CreatedAt }},
		ConnUpdatedAt:    {Column: "updated_at", Scan: func(r *Connection) any { return &r.UpdatedAt }, Value: func(r *Connection) any { return r.UpdatedAt }},
		ConnTargetDB:     {Column: "target_db", Scan: func(r *Connection) any { return &r.TargetDB }, Value: func(r *Connection) any { return r.TargetDB }},
	})
}

// --- workspaces ----------------------------------------------------------------

// Workspace is a named group of connections (Objective 5).
type Workspace struct {
	ID        int64
	Name      string
	CreatedAt int64
}

type WorkspaceField string

const (
	WsID        WorkspaceField = "id"
	WsName      WorkspaceField = "name"
	WsCreatedAt WorkspaceField = "created_at"
)

func newWorkspaces(conn dao.DataConn) *dao.Schema[*Workspace, WorkspaceField, Sort, int64] {
	return schema(conn, "workspaces", WsID, map[WorkspaceField]dao.Field[*Workspace]{
		WsID:        {Column: "id", Scan: func(r *Workspace) any { return &r.ID }},
		WsName:      {Column: "name", Scan: func(r *Workspace) any { return &r.Name }, Value: func(r *Workspace) any { return r.Name }},
		WsCreatedAt: {Column: "created_at", Scan: func(r *Workspace) any { return &r.CreatedAt }, Value: func(r *Workspace) any { return r.CreatedAt }},
	})
}

// WorkspaceConn links a workspace to a connection (surrogate id; the pair is
// UNIQUE).
type WorkspaceConn struct {
	ID           int64
	WorkspaceID  int64
	ConnectionID int64
}

type WsConnField string

const (
	WcID     WsConnField = "id"
	WcWsID   WsConnField = "workspace_id"
	WcConnID WsConnField = "connection_id"
)

func newWorkspaceConns(conn dao.DataConn) *dao.Schema[*WorkspaceConn, WsConnField, Sort, int64] {
	return schema(conn, "workspace_connections", WcID, map[WsConnField]dao.Field[*WorkspaceConn]{
		WcID:     {Column: "id", Scan: func(r *WorkspaceConn) any { return &r.ID }},
		WcWsID:   {Column: "workspace_id", Scan: func(r *WorkspaceConn) any { return &r.WorkspaceID }, Value: func(r *WorkspaceConn) any { return r.WorkspaceID }},
		WcConnID: {Column: "connection_id", Scan: func(r *WorkspaceConn) any { return &r.ConnectionID }, Value: func(r *WorkspaceConn) any { return r.ConnectionID }},
	})
}

// --- grants ---------------------------------------------------------------------

// Grant gives a user a role on one connection (Objectives 13-15).
type Grant struct {
	ID           int64
	UserID       int64
	ConnectionID int64
	Role         string
	GrantedBy    int64
	CreatedAt    int64
}

type GrantField string

const (
	GrantID        GrantField = "id"
	GrantUserID    GrantField = "user_id"
	GrantConnID    GrantField = "connection_id"
	GrantRole      GrantField = "role"
	GrantGrantedBy GrantField = "granted_by"
	GrantCreatedAt GrantField = "created_at"
)

func newGrants(conn dao.DataConn) *dao.Schema[*Grant, GrantField, Sort, int64] {
	return schema(conn, "grants", GrantID, map[GrantField]dao.Field[*Grant]{
		GrantID:        {Column: "id", Scan: func(r *Grant) any { return &r.ID }},
		GrantUserID:    {Column: "user_id", Scan: func(r *Grant) any { return &r.UserID }, Value: func(r *Grant) any { return r.UserID }},
		GrantConnID:    {Column: "connection_id", Scan: func(r *Grant) any { return &r.ConnectionID }, Value: func(r *Grant) any { return r.ConnectionID }},
		GrantRole:      {Column: "role", Scan: func(r *Grant) any { return &r.Role }, Value: func(r *Grant) any { return r.Role }},
		GrantGrantedBy: {Column: "granted_by", Scan: func(r *Grant) any { return &r.GrantedBy }, Value: func(r *Grant) any { return r.GrantedBy }},
		GrantCreatedAt: {Column: "created_at", Scan: func(r *Grant) any { return &r.CreatedAt }, Value: func(r *Grant) any { return r.CreatedAt }},
	})
}

// --- sessions -------------------------------------------------------------------

// Session is a handed-out login token (Objective 20; issuance in M3). Only
// the token's hash is stored.
type Session struct {
	ID        int64
	TokenHash []byte
	UserID    int64
	IP        string
	CreatedAt int64
	ExpiresAt int64
	Revoked   int64 // 0/1 flag
}

type SessionField string

const (
	SessID        SessionField = "id"
	SessTokenHash SessionField = "token_hash"
	SessUserID    SessionField = "user_id"
	SessIP        SessionField = "ip"
	SessCreatedAt SessionField = "created_at"
	SessExpiresAt SessionField = "expires_at"
	SessRevoked   SessionField = "revoked"
)

func newSessions(conn dao.DataConn) *dao.Schema[*Session, SessionField, Sort, int64] {
	return schema(conn, "sessions", SessID, map[SessionField]dao.Field[*Session]{
		SessID:        {Column: "id", Scan: func(r *Session) any { return &r.ID }},
		SessTokenHash: {Column: "token_hash", Scan: func(r *Session) any { return &r.TokenHash }, Value: func(r *Session) any { return r.TokenHash }},
		SessUserID:    {Column: "user_id", Scan: func(r *Session) any { return &r.UserID }, Value: func(r *Session) any { return r.UserID }},
		SessIP:        {Column: "ip", Scan: func(r *Session) any { return &r.IP }, Value: func(r *Session) any { return r.IP }},
		SessCreatedAt: {Column: "created_at", Scan: func(r *Session) any { return &r.CreatedAt }, Value: func(r *Session) any { return r.CreatedAt }},
		SessExpiresAt: {Column: "expires_at", Scan: func(r *Session) any { return &r.ExpiresAt }, Value: func(r *Session) any { return r.ExpiresAt }},
		SessRevoked:   {Column: "revoked", Scan: func(r *Session) any { return &r.Revoked }, Value: func(r *Session) any { return r.Revoked }},
	})
}

// --- script history --------------------------------------------------------------

// HistoryEntry is a recallable executed script (Objective 5; toggleable —
// the audit log is the always-on record).
type HistoryEntry struct {
	ID           int64
	UserID       int64
	ConnectionID int64
	IP           string
	Script       string
	StartedAt    int64
	DurationMS   int64
	RowCount     int64
	Status       HistoryStatus
	Error        string
	// TxID correlates this statement with the transaction that decides
	// whether its effect survives. Empty for autocommit, where the
	// statement's own return already settles that.
	TxID string
}

type HistoryField string

const (
	HistID     HistoryField = "id"
	HistUserID HistoryField = "user_id"
	HistConnID HistoryField = "connection_id"
	HistIP     HistoryField = "ip"
	HistScript HistoryField = "script"
	// HistStartedAt is unix SECONDS (written with time.Time.Unix()).
	// Readers that assume milliseconds date every row to January 1970.
	HistStartedAt  HistoryField = "started_at"
	HistDurationMS HistoryField = "duration_ms"
	HistRowCount   HistoryField = "row_count"
	HistStatus     HistoryField = "status"
	HistError      HistoryField = "error"
	HistTxID       HistoryField = "tx_id"
)

// HistByID orders history by insertion, so the repair sweep can page it.
const HistByID Sort = "id"

func newHistory(conn dao.DataConn) *dao.Schema[*HistoryEntry, HistoryField, Sort, int64] {
	return sortableSchema(conn, "script_history", HistID,
		map[Sort]string{HistByID: "id"},
		map[HistoryField]dao.Field[*HistoryEntry]{
			HistID:         {Column: "id", Scan: func(r *HistoryEntry) any { return &r.ID }},
			HistUserID:     {Column: "user_id", Scan: func(r *HistoryEntry) any { return &r.UserID }, Value: func(r *HistoryEntry) any { return r.UserID }},
			HistConnID:     {Column: "connection_id", Scan: func(r *HistoryEntry) any { return &r.ConnectionID }, Value: func(r *HistoryEntry) any { return r.ConnectionID }},
			HistIP:         {Column: "ip", Scan: func(r *HistoryEntry) any { return &r.IP }, Value: func(r *HistoryEntry) any { return r.IP }},
			HistScript:     {Column: "script", Scan: func(r *HistoryEntry) any { return &r.Script }, Value: func(r *HistoryEntry) any { return r.Script }},
			HistStartedAt:  {Column: "started_at", Scan: func(r *HistoryEntry) any { return &r.StartedAt }, Value: func(r *HistoryEntry) any { return r.StartedAt }},
			HistDurationMS: {Column: "duration_ms", Scan: func(r *HistoryEntry) any { return &r.DurationMS }, Value: func(r *HistoryEntry) any { return r.DurationMS }},
			HistRowCount:   {Column: "row_count", Scan: func(r *HistoryEntry) any { return &r.RowCount }, Value: func(r *HistoryEntry) any { return r.RowCount }},
			HistStatus:     {Column: "status", Scan: func(r *HistoryEntry) any { return &r.Status }, Value: func(r *HistoryEntry) any { return string(r.Status) }},
			HistError:      {Column: "error", Scan: func(r *HistoryEntry) any { return &r.Error }, Value: func(r *HistoryEntry) any { return r.Error }},
			HistTxID:       {Column: "tx_id", Scan: func(r *HistoryEntry) any { return &r.TxID }, Value: func(r *HistoryEntry) any { return r.TxID }},
		})
}

// --- audit log --------------------------------------------------------------------

// AuditEntry is the always-on compliance record (Objective 20). UserID 0
// means "no authenticated user" (e.g. a failed login) — deliberately no FK
// so auditing can never fail.
type AuditEntry struct {
	ID        int64
	UserID    int64
	IP        string
	Action    string
	Detail    string
	CreatedAt int64
	// TxID ties a boundary or in-transaction record to its transaction, so
	// the trail can be read per-transaction rather than reconstructed by
	// parsing Detail.
	TxID string
}

type AuditField string

const (
	AuditID        AuditField = "id"
	AuditUserID    AuditField = "user_id"
	AuditIP        AuditField = "ip"
	AuditAction    AuditField = "action"
	AuditDetail    AuditField = "detail"
	AuditCreatedAt AuditField = "created_at"
	AuditTxID      AuditField = "tx_id"
)

func newAudit(conn dao.DataConn) *dao.Schema[*AuditEntry, AuditField, Sort, int64] {
	return schema(conn, "audit_log", AuditID, map[AuditField]dao.Field[*AuditEntry]{
		AuditID:        {Column: "id", Scan: func(r *AuditEntry) any { return &r.ID }},
		AuditUserID:    {Column: "user_id", Scan: func(r *AuditEntry) any { return &r.UserID }, Value: func(r *AuditEntry) any { return r.UserID }},
		AuditIP:        {Column: "ip", Scan: func(r *AuditEntry) any { return &r.IP }, Value: func(r *AuditEntry) any { return r.IP }},
		AuditAction:    {Column: "action", Scan: func(r *AuditEntry) any { return &r.Action }, Value: func(r *AuditEntry) any { return r.Action }},
		AuditDetail:    {Column: "detail", Scan: func(r *AuditEntry) any { return &r.Detail }, Value: func(r *AuditEntry) any { return r.Detail }},
		AuditCreatedAt: {Column: "created_at", Scan: func(r *AuditEntry) any { return &r.CreatedAt }, Value: func(r *AuditEntry) any { return r.CreatedAt }},
		AuditTxID:      {Column: "tx_id", Scan: func(r *AuditEntry) any { return &r.TxID }, Value: func(r *AuditEntry) any { return r.TxID }},
	})
}

// --- ip allowlist -----------------------------------------------------------------

// AllowedIP is a persisted allowlist entry (Objective 21) — the config file's
// list seeds runtime state; managed entries live here.
type AllowedIP struct {
	ID        int64
	CIDR      string
	Note      string
	CreatedBy int64
	CreatedAt int64
}

type AllowedIPField string

const (
	IPID        AllowedIPField = "id"
	IPCIDR      AllowedIPField = "cidr"
	IPNote      AllowedIPField = "note"
	IPCreatedBy AllowedIPField = "created_by"
	IPCreatedAt AllowedIPField = "created_at"
)

func newAllowedIPs(conn dao.DataConn) *dao.Schema[*AllowedIP, AllowedIPField, Sort, int64] {
	return schema(conn, "ip_allowlist", IPID, map[AllowedIPField]dao.Field[*AllowedIP]{
		IPID:        {Column: "id", Scan: func(r *AllowedIP) any { return &r.ID }},
		IPCIDR:      {Column: "cidr", Scan: func(r *AllowedIP) any { return &r.CIDR }, Value: func(r *AllowedIP) any { return r.CIDR }},
		IPNote:      {Column: "note", Scan: func(r *AllowedIP) any { return &r.Note }, Value: func(r *AllowedIP) any { return r.Note }},
		IPCreatedBy: {Column: "created_by", Scan: func(r *AllowedIP) any { return &r.CreatedBy }, Value: func(r *AllowedIP) any { return r.CreatedBy }},
		IPCreatedAt: {Column: "created_at", Scan: func(r *AllowedIP) any { return &r.CreatedAt }, Value: func(r *AllowedIP) any { return r.CreatedAt }},
	})
}

// UserIP is one user_ip_allowlist row (ADR-0075 §4): the per-user layer of
// the front door's two-layer IP model. A front-door login must pass BOTH the
// global allowlist and the connecting user's rows, and a PAT's allowed_ips
// must be a subset of these. Managed self-service (own rows) or by an admin.
type UserIP struct {
	ID        int64
	UserID    int64
	CIDR      string
	Label     string
	CreatedAt int64
}

type UserIPField string

const (
	UIPID        UserIPField = "id"
	UIPUserID    UserIPField = "user_id"
	UIPCIDR      UserIPField = "cidr"
	UIPLabel     UserIPField = "label"
	UIPCreatedAt UserIPField = "created_at"
)

func newUserIPs(conn dao.DataConn) *dao.Schema[*UserIP, UserIPField, Sort, int64] {
	return schema(conn, "user_ip_allowlist", UIPID, map[UserIPField]dao.Field[*UserIP]{
		UIPID:        {Column: "id", Scan: func(r *UserIP) any { return &r.ID }},
		UIPUserID:    {Column: "user_id", Scan: func(r *UserIP) any { return &r.UserID }, Value: func(r *UserIP) any { return r.UserID }},
		UIPCIDR:      {Column: "cidr", Scan: func(r *UserIP) any { return &r.CIDR }, Value: func(r *UserIP) any { return r.CIDR }},
		UIPLabel:     {Column: "label", Scan: func(r *UserIP) any { return &r.Label }, Value: func(r *UserIP) any { return r.Label }},
		UIPCreatedAt: {Column: "created_at", Scan: func(r *UserIP) any { return &r.CreatedAt }, Value: func(r *UserIP) any { return r.CreatedAt }},
	})
}

// --- keyslots ---------------------------------------------------------------------

// Keyslot is one row of the keyslots table: a copy of the install master key,
// wrapped by something other than a user passphrase (ADR-0087).
//
// It is the SIBLING of users.mk_wrapped, and deliberately the same shape —
// Wrapped is the identical nonce-prefixed AES-GCM blob, in a BLOB/BYTEA column,
// so a reader written against one works against the other. Amendment 1 A1.1
// chose a table over a store_meta row for exactly that reason: store_meta.value
// is TEXT, and base64 there would have made these two the same secret in two
// representations.
type Keyslot struct {
	// Kind names which non-user key opens this slot. 'service' is the only
	// one the schema permits today; 'tpm' and 'kms' are the anticipated
	// growth and are refused until something can actually unwrap them.
	Kind string
	// Wrapped is the master key sealed to this slot's KEK: nonce ‖ ciphertext.
	Wrapped []byte
	// AADVersion records WHICH binding sealed it (ADR-0087 §3), so a rotation
	// can tell an old blob from a new one instead of failing to open and
	// guessing why.
	AADVersion string
	CreatedBy  int64
	CreatedAt  int64
}

type KeyslotField string

const (
	KeyslotKind       KeyslotField = "kind"
	KeyslotWrapped    KeyslotField = "wrapped"
	KeyslotAADVersion KeyslotField = "aad_version"
	KeyslotCreatedBy  KeyslotField = "created_by"
	KeyslotCreatedAt  KeyslotField = "created_at"
)

// KeyslotKindService is the only kind the v14 CHECK permits.
const KeyslotKindService = "service"

func newKeyslots(conn dao.DataConn) *dao.Schema[*Keyslot, KeyslotField, Sort, string] {
	return dao.New(conn,
		dao.Table[*Keyslot, KeyslotField, Sort, string]("keyslots"),
		dao.ID[*Keyslot, KeyslotField, Sort, string](KeyslotKind),
		dao.Conflict[*Keyslot, KeyslotField, Sort, string](KeyslotKind),
		dao.Fields[*Keyslot, KeyslotField, Sort, string](map[KeyslotField]dao.Field[*Keyslot]{
			KeyslotKind:       {Column: "kind", Scan: func(r *Keyslot) any { return &r.Kind }, Value: func(r *Keyslot) any { return r.Kind }},
			KeyslotWrapped:    {Column: "wrapped", Scan: func(r *Keyslot) any { return &r.Wrapped }, Value: func(r *Keyslot) any { return r.Wrapped }},
			KeyslotAADVersion: {Column: "aad_version", Scan: func(r *Keyslot) any { return &r.AADVersion }, Value: func(r *Keyslot) any { return r.AADVersion }},
			KeyslotCreatedBy:  {Column: "created_by", Scan: func(r *Keyslot) any { return &r.CreatedBy }, Value: func(r *Keyslot) any { return r.CreatedBy }},
			KeyslotCreatedAt:  {Column: "created_at", Scan: func(r *Keyslot) any { return &r.CreatedAt }, Value: func(r *Keyslot) any { return r.CreatedAt }},
		}),
	)
}

// --- store_meta -------------------------------------------------------------------

// MetaKV is one store_meta row: install-scoped key/value state (schema
// provenance, install id, M3 master-key material).
type MetaKV struct {
	Key   string
	Value string
}

type MetaKVField string

const (
	KVKey   MetaKVField = "key"
	KVValue MetaKVField = "value"
)

func newKV(conn dao.DataConn) *dao.Schema[*MetaKV, MetaKVField, Sort, string] {
	return dao.New(conn,
		dao.Table[*MetaKV, MetaKVField, Sort, string]("store_meta"),
		dao.ID[*MetaKV, MetaKVField, Sort, string](KVKey),
		dao.Conflict[*MetaKV, MetaKVField, Sort, string](KVKey),
		dao.Fields[*MetaKV, MetaKVField, Sort, string](map[MetaKVField]dao.Field[*MetaKV]{
			KVKey:   {Column: "key", Scan: func(r *MetaKV) any { return &r.Key }, Value: func(r *MetaKV) any { return r.Key }},
			KVValue: {Column: "value", Scan: func(r *MetaKV) any { return &r.Value }, Value: func(r *MetaKV) any { return r.Value }},
		}),
	)
}

// PAT is a Personal Access Token — the front door's credential (ADR-0075 §4).
//
// Distinct from Session by design: named, deliberately long-lived, and pasted
// into a DSN by a person. A session is anonymous, short-lived, and held by a
// client. Sharing one table would put an "and not a PAT" clause on every
// session query, which is the sort of condition that gets forgotten once.
type PAT struct {
	ID int64
	// Selector is the stable, indexed lookup key. Authentication finds a row
	// by equality on this and never by scanning hashes: a scan would make
	// lookup cost depend on how many tokens exist, and the uniform failure
	// shape depends on that cost being flat.
	Selector string
	// SecretHash is SHA-256 of the secret half. The secret itself is shown
	// once at creation and never stored.
	SecretHash []byte
	UserID     int64
	Name       string
	// AllowedIPs is this token's own narrowing, canonicalized on write.
	// EMPTY means it inherits the admission set (ADR-0075 Amendment 1) —
	// not "nowhere", which would make an ordinary token useless.
	AllowedIPs string
	CreatedAt  int64
	ExpiresAt  int64
	// LastUsedAt is written coalesced, never once per statement.
	LastUsedAt int64
	Revoked    int64 // 0/1 flag
	// ConnID is the ONE connection this token may reach (ADR-0086 §1).
	//
	// Binding the credential is what dissolves the connection-name vs
	// target-database-name ambiguity: two connections targeting a database
	// called `test` — a local one and a PRODUCTION one — are told apart by
	// WHICH TOKEN was presented, not by a string the client chose. It also
	// shrinks blast radius on its own merits: a stolen token reaches one
	// connection rather than every connection its owner is granted.
	//
	// ZERO IS A TOMBSTONE, NOT A VALUE. Pre-v13 rows carry 0 because nothing
	// can derive which connection they were for; the migration revokes them
	// and the auth path refuses 0 independently of Revoked, so a row
	// un-revoked by hand cannot come back as an unscoped token.
	ConnID int64
	// DebugCleartext marks a token mintable ONLY while the daemon serves
	// cleartext (ADR-0086 §10). Such a token REQUIRES a non-empty AllowedIPs,
	// which is then its whole admission gate rather than a narrowing of its
	// owner's — the inherited set is not consulted — and it is REFUSED on a
	// TLS listener, so the relaxed perimeter cannot leave the debugging mode
	// it was minted for. 0/1, matching users.disabled.
	DebugCleartext int64
}

// IsRevoked reports whether the token has been revoked.
func (p *PAT) IsRevoked() bool { return p.Revoked != 0 }

// PATField names a column of pats.
type PATField = string

// PAT columns.
const (
	PATID         PATField = "id"
	PATSelector   PATField = "selector"
	PATSecretHash PATField = "secret_hash"
	PATUserID     PATField = "user_id"
	PATName       PATField = "name"
	PATAllowedIPs PATField = "allowed_ips"
	PATCreatedAt  PATField = "created_at"
	PATExpiresAt  PATField = "expires_at"
	PATLastUsedAt PATField = "last_used_at"
	PATRevoked    PATField = "revoked"

	PATConnID         PATField = "conn_id"
	PATDebugCleartext PATField = "debug_cleartext"
)

func newPATs(conn dao.DataConn) *dao.Schema[*PAT, PATField, Sort, int64] {
	return schema(conn, "pats", PATID, map[PATField]dao.Field[*PAT]{
		PATID:         {Column: "id", Scan: func(r *PAT) any { return &r.ID }},
		PATSelector:   {Column: "selector", Scan: func(r *PAT) any { return &r.Selector }, Value: func(r *PAT) any { return r.Selector }},
		PATSecretHash: {Column: "secret_hash", Scan: func(r *PAT) any { return &r.SecretHash }, Value: func(r *PAT) any { return r.SecretHash }},
		PATUserID:     {Column: "user_id", Scan: func(r *PAT) any { return &r.UserID }, Value: func(r *PAT) any { return r.UserID }},
		PATName:       {Column: "name", Scan: func(r *PAT) any { return &r.Name }, Value: func(r *PAT) any { return r.Name }},
		PATAllowedIPs: {Column: "allowed_ips", Scan: func(r *PAT) any { return &r.AllowedIPs }, Value: func(r *PAT) any { return r.AllowedIPs }},
		PATCreatedAt:  {Column: "created_at", Scan: func(r *PAT) any { return &r.CreatedAt }, Value: func(r *PAT) any { return r.CreatedAt }},
		PATExpiresAt:  {Column: "expires_at", Scan: func(r *PAT) any { return &r.ExpiresAt }, Value: func(r *PAT) any { return r.ExpiresAt }},
		PATLastUsedAt: {Column: "last_used_at", Scan: func(r *PAT) any { return &r.LastUsedAt }, Value: func(r *PAT) any { return r.LastUsedAt }},
		PATRevoked:    {Column: "revoked", Scan: func(r *PAT) any { return &r.Revoked }, Value: func(r *PAT) any { return r.Revoked }},

		PATConnID:         {Column: "conn_id", Scan: func(r *PAT) any { return &r.ConnID }, Value: func(r *PAT) any { return r.ConnID }},
		PATDebugCleartext: {Column: "debug_cleartext", Scan: func(r *PAT) any { return &r.DebugCleartext }, Value: func(r *PAT) any { return r.DebugCleartext }},
	})
}
