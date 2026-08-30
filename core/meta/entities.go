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
	Debug     int64
	CreatedBy int64
	CreatedAt int64
	UpdatedAt int64
}

// IsDebug reports whether the connection carries the debug profile.
func (c *Connection) IsDebug() bool { return c.Debug != 0 }

type ConnField string

const (
	ConnID        ConnField = "id"
	ConnName      ConnField = "name"
	ConnEngine    ConnField = "engine"
	ConnDSNEnc    ConnField = "dsn_enc"
	ConnProfile   ConnField = "profile"
	ConnDebug     ConnField = "debug"
	ConnCreatedBy ConnField = "created_by"
	ConnCreatedAt ConnField = "created_at"
	ConnUpdatedAt ConnField = "updated_at"
)

func newConnections(conn dao.DataConn) *dao.Schema[*Connection, ConnField, Sort, int64] {
	return schema(conn, "connections", ConnID, map[ConnField]dao.Field[*Connection]{
		ConnID:        {Column: "id", Scan: func(r *Connection) any { return &r.ID }},
		ConnName:      {Column: "name", Scan: func(r *Connection) any { return &r.Name }, Value: func(r *Connection) any { return r.Name }},
		ConnEngine:    {Column: "engine", Scan: func(r *Connection) any { return &r.Engine }, Value: func(r *Connection) any { return r.Engine }},
		ConnProfile:   {Column: "profile", Scan: func(r *Connection) any { return &r.Profile }, Value: func(r *Connection) any { return r.Profile }},
		ConnDebug:     {Column: "debug", Scan: func(r *Connection) any { return &r.Debug }, Value: func(r *Connection) any { return r.Debug }},
		ConnDSNEnc:    {Column: "dsn_enc", Scan: func(r *Connection) any { return &r.DSNEnc }, Value: func(r *Connection) any { return r.DSNEnc }},
		ConnCreatedBy: {Column: "created_by", Scan: func(r *Connection) any { return &r.CreatedBy }, Value: func(r *Connection) any { return r.CreatedBy }},
		ConnCreatedAt: {Column: "created_at", Scan: func(r *Connection) any { return &r.CreatedAt }, Value: func(r *Connection) any { return r.CreatedAt }},
		ConnUpdatedAt: {Column: "updated_at", Scan: func(r *Connection) any { return &r.UpdatedAt }, Value: func(r *Connection) any { return r.UpdatedAt }},
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
	Status       string
	Error        string
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
)

func newHistory(conn dao.DataConn) *dao.Schema[*HistoryEntry, HistoryField, Sort, int64] {
	return schema(conn, "script_history", HistID, map[HistoryField]dao.Field[*HistoryEntry]{
		HistID:         {Column: "id", Scan: func(r *HistoryEntry) any { return &r.ID }},
		HistUserID:     {Column: "user_id", Scan: func(r *HistoryEntry) any { return &r.UserID }, Value: func(r *HistoryEntry) any { return r.UserID }},
		HistConnID:     {Column: "connection_id", Scan: func(r *HistoryEntry) any { return &r.ConnectionID }, Value: func(r *HistoryEntry) any { return r.ConnectionID }},
		HistIP:         {Column: "ip", Scan: func(r *HistoryEntry) any { return &r.IP }, Value: func(r *HistoryEntry) any { return r.IP }},
		HistScript:     {Column: "script", Scan: func(r *HistoryEntry) any { return &r.Script }, Value: func(r *HistoryEntry) any { return r.Script }},
		HistStartedAt:  {Column: "started_at", Scan: func(r *HistoryEntry) any { return &r.StartedAt }, Value: func(r *HistoryEntry) any { return r.StartedAt }},
		HistDurationMS: {Column: "duration_ms", Scan: func(r *HistoryEntry) any { return &r.DurationMS }, Value: func(r *HistoryEntry) any { return r.DurationMS }},
		HistRowCount:   {Column: "row_count", Scan: func(r *HistoryEntry) any { return &r.RowCount }, Value: func(r *HistoryEntry) any { return r.RowCount }},
		HistStatus:     {Column: "status", Scan: func(r *HistoryEntry) any { return &r.Status }, Value: func(r *HistoryEntry) any { return r.Status }},
		HistError:      {Column: "error", Scan: func(r *HistoryEntry) any { return &r.Error }, Value: func(r *HistoryEntry) any { return r.Error }},
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
}

type AuditField string

const (
	AuditID        AuditField = "id"
	AuditUserID    AuditField = "user_id"
	AuditIP        AuditField = "ip"
	AuditAction    AuditField = "action"
	AuditDetail    AuditField = "detail"
	AuditCreatedAt AuditField = "created_at"
)

func newAudit(conn dao.DataConn) *dao.Schema[*AuditEntry, AuditField, Sort, int64] {
	return schema(conn, "audit_log", AuditID, map[AuditField]dao.Field[*AuditEntry]{
		AuditID:        {Column: "id", Scan: func(r *AuditEntry) any { return &r.ID }},
		AuditUserID:    {Column: "user_id", Scan: func(r *AuditEntry) any { return &r.UserID }, Value: func(r *AuditEntry) any { return r.UserID }},
		AuditIP:        {Column: "ip", Scan: func(r *AuditEntry) any { return &r.IP }, Value: func(r *AuditEntry) any { return r.IP }},
		AuditAction:    {Column: "action", Scan: func(r *AuditEntry) any { return &r.Action }, Value: func(r *AuditEntry) any { return r.Action }},
		AuditDetail:    {Column: "detail", Scan: func(r *AuditEntry) any { return &r.Detail }, Value: func(r *AuditEntry) any { return r.Detail }},
		AuditCreatedAt: {Column: "created_at", Scan: func(r *AuditEntry) any { return &r.CreatedAt }, Value: func(r *AuditEntry) any { return r.CreatedAt }},
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
