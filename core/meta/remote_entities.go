package meta

import "github.com/yongjohnlee80/golib/dao"

// REMOTE ACCESS — the tables schema script 000004_update_remote_access adds
// (sql/deployments/*/000004_update_remote_access.sql says what each holds).
//
// "None" is 0 or "" throughout, as the rest of this schema writes it: a
// revoked_at of 0 is a live row, a user id of 0 names nobody.

// --- user_ssh_keys ---------------------------------------------------------------

// UserSSHKey is one SSH public key registered on a user's profile: the key a
// remote connection authenticates with. A fingerprint names one
// LIVE key across all users; a revoked row stays as history.
type UserSSHKey struct {
	ID          int64
	UserID      int64
	Label       string
	PublicKey   string // authorized-key text
	Fingerprint string // SHA256:…
	AddedBy     int64
	CreatedAt   int64
	LastUsedAt  int64
	RevokedAt   int64 // 0 while live
	RevokedBy   int64
}

type UserSSHKeyField string

const (
	SSHKeyID          UserSSHKeyField = "id"
	SSHKeyUserID      UserSSHKeyField = "user_id"
	SSHKeyLabel       UserSSHKeyField = "label"
	SSHKeyPublicKey   UserSSHKeyField = "public_key"
	SSHKeyFingerprint UserSSHKeyField = "fingerprint"
	SSHKeyAddedBy     UserSSHKeyField = "added_by"
	SSHKeyCreatedAt   UserSSHKeyField = "created_at"
	SSHKeyLastUsedAt  UserSSHKeyField = "last_used_at"
	SSHKeyRevokedAt   UserSSHKeyField = "revoked_at"
	SSHKeyRevokedBy   UserSSHKeyField = "revoked_by"
)

func newSSHKeys(conn dao.DataConn) *dao.Schema[*UserSSHKey, UserSSHKeyField, Sort, int64] {
	return schema(conn, "user_ssh_keys", SSHKeyID, map[UserSSHKeyField]dao.Field[*UserSSHKey]{
		SSHKeyID:          {Column: "id", Scan: func(r *UserSSHKey) any { return &r.ID }},
		SSHKeyUserID:      {Column: "user_id", Scan: func(r *UserSSHKey) any { return &r.UserID }, Value: func(r *UserSSHKey) any { return r.UserID }},
		SSHKeyLabel:       {Column: "label", Scan: func(r *UserSSHKey) any { return &r.Label }, Value: func(r *UserSSHKey) any { return r.Label }},
		SSHKeyPublicKey:   {Column: "public_key", Scan: func(r *UserSSHKey) any { return &r.PublicKey }, Value: func(r *UserSSHKey) any { return r.PublicKey }},
		SSHKeyFingerprint: {Column: "fingerprint", Scan: func(r *UserSSHKey) any { return &r.Fingerprint }, Value: func(r *UserSSHKey) any { return r.Fingerprint }},
		SSHKeyAddedBy:     {Column: "added_by", Scan: func(r *UserSSHKey) any { return &r.AddedBy }, Value: func(r *UserSSHKey) any { return r.AddedBy }},
		SSHKeyCreatedAt:   {Column: "created_at", Scan: func(r *UserSSHKey) any { return &r.CreatedAt }, Value: func(r *UserSSHKey) any { return r.CreatedAt }},
		SSHKeyLastUsedAt:  {Column: "last_used_at", Scan: func(r *UserSSHKey) any { return &r.LastUsedAt }, Value: func(r *UserSSHKey) any { return r.LastUsedAt }},
		SSHKeyRevokedAt:   {Column: "revoked_at", Scan: func(r *UserSSHKey) any { return &r.RevokedAt }, Value: func(r *UserSSHKey) any { return r.RevokedAt }},
		SSHKeyRevokedBy:   {Column: "revoked_by", Scan: func(r *UserSSHKey) any { return &r.RevokedBy }, Value: func(r *UserSSHKey) any { return r.RevokedBy }},
	})
}

// --- remote_devices -------------------------------------------------------------

// RemoteDevice is the device an SSH key enrolled on its first remote connect:
// its public key, proven on every connect. One live device per key.
type RemoteDevice struct {
	ID           int64
	SSHKeyID     int64
	UserID       int64
	PublicKey    string
	Fingerprint  string
	EnrolledAt   int64
	EnrolledIP   string
	KeyCreatedAt int64 // what rotation ages
	LastSeenAt   int64
	RevokedAt    int64 // 0 while live
	RevokedBy    int64
}

type RemoteDeviceField string

const (
	DevID           RemoteDeviceField = "id"
	DevSSHKeyID     RemoteDeviceField = "ssh_key_id"
	DevUserID       RemoteDeviceField = "user_id"
	DevPublicKey    RemoteDeviceField = "device_public_key"
	DevFingerprint  RemoteDeviceField = "device_fingerprint"
	DevEnrolledAt   RemoteDeviceField = "enrolled_at"
	DevEnrolledIP   RemoteDeviceField = "enrolled_ip"
	DevKeyCreatedAt RemoteDeviceField = "key_created_at"
	DevLastSeenAt   RemoteDeviceField = "last_seen_at"
	DevRevokedAt    RemoteDeviceField = "revoked_at"
	DevRevokedBy    RemoteDeviceField = "revoked_by"
)

func newRemoteDevices(conn dao.DataConn) *dao.Schema[*RemoteDevice, RemoteDeviceField, Sort, int64] {
	return schema(conn, "remote_devices", DevID, map[RemoteDeviceField]dao.Field[*RemoteDevice]{
		DevID:           {Column: "id", Scan: func(r *RemoteDevice) any { return &r.ID }},
		DevSSHKeyID:     {Column: "ssh_key_id", Scan: func(r *RemoteDevice) any { return &r.SSHKeyID }, Value: func(r *RemoteDevice) any { return r.SSHKeyID }},
		DevUserID:       {Column: "user_id", Scan: func(r *RemoteDevice) any { return &r.UserID }, Value: func(r *RemoteDevice) any { return r.UserID }},
		DevPublicKey:    {Column: "device_public_key", Scan: func(r *RemoteDevice) any { return &r.PublicKey }, Value: func(r *RemoteDevice) any { return r.PublicKey }},
		DevFingerprint:  {Column: "device_fingerprint", Scan: func(r *RemoteDevice) any { return &r.Fingerprint }, Value: func(r *RemoteDevice) any { return r.Fingerprint }},
		DevEnrolledAt:   {Column: "enrolled_at", Scan: func(r *RemoteDevice) any { return &r.EnrolledAt }, Value: func(r *RemoteDevice) any { return r.EnrolledAt }},
		DevEnrolledIP:   {Column: "enrolled_ip", Scan: func(r *RemoteDevice) any { return &r.EnrolledIP }, Value: func(r *RemoteDevice) any { return r.EnrolledIP }},
		DevKeyCreatedAt: {Column: "key_created_at", Scan: func(r *RemoteDevice) any { return &r.KeyCreatedAt }, Value: func(r *RemoteDevice) any { return r.KeyCreatedAt }},
		DevLastSeenAt:   {Column: "last_seen_at", Scan: func(r *RemoteDevice) any { return &r.LastSeenAt }, Value: func(r *RemoteDevice) any { return r.LastSeenAt }},
		DevRevokedAt:    {Column: "revoked_at", Scan: func(r *RemoteDevice) any { return &r.RevokedAt }, Value: func(r *RemoteDevice) any { return r.RevokedAt }},
		DevRevokedBy:    {Column: "revoked_by", Scan: func(r *RemoteDevice) any { return &r.RevokedBy }, Value: func(r *RemoteDevice) any { return r.RevokedBy }},
	})
}

// --- remote_device_ips ----------------------------------------------------------

// RemoteDeviceIP is an address a device has connected from: the first
// connect from a new one is the "new IP for a device" event.
type RemoteDeviceIP struct {
	ID          int64
	DeviceID    int64
	IP          string
	FirstSeenAt int64
	LastSeenAt  int64
}

type RemoteDeviceIPField string

const (
	DevIPID          RemoteDeviceIPField = "id"
	DevIPDeviceID    RemoteDeviceIPField = "device_id"
	DevIPIP          RemoteDeviceIPField = "ip"
	DevIPFirstSeenAt RemoteDeviceIPField = "first_seen_at"
	DevIPLastSeenAt  RemoteDeviceIPField = "last_seen_at"
)

func newRemoteDeviceIPs(conn dao.DataConn) *dao.Schema[*RemoteDeviceIP, RemoteDeviceIPField, Sort, int64] {
	return schema(conn, "remote_device_ips", DevIPID, map[RemoteDeviceIPField]dao.Field[*RemoteDeviceIP]{
		DevIPID:          {Column: "id", Scan: func(r *RemoteDeviceIP) any { return &r.ID }},
		DevIPDeviceID:    {Column: "device_id", Scan: func(r *RemoteDeviceIP) any { return &r.DeviceID }, Value: func(r *RemoteDeviceIP) any { return r.DeviceID }},
		DevIPIP:          {Column: "ip", Scan: func(r *RemoteDeviceIP) any { return &r.IP }, Value: func(r *RemoteDeviceIP) any { return r.IP }},
		DevIPFirstSeenAt: {Column: "first_seen_at", Scan: func(r *RemoteDeviceIP) any { return &r.FirstSeenAt }, Value: func(r *RemoteDeviceIP) any { return r.FirstSeenAt }},
		DevIPLastSeenAt:  {Column: "last_seen_at", Scan: func(r *RemoteDeviceIP) any { return &r.LastSeenAt }, Value: func(r *RemoteDeviceIP) any { return r.LastSeenAt }},
	})
}

// --- remote_ip_blocks -----------------------------------------------------------

// RemoteIPBlock is one source prefix's failure count and the block it set:
// an IPv4 /32 or IPv6 /64, keyed by its prefix.
type RemoteIPBlock struct {
	Prefix              string
	ConsecutiveFailures int64
	LastFailureAt       int64
	BlockedUntil        int64 // 0 when not blocked
	UnblockedBy         int64
	UnblockedAt         int64
}

type RemoteIPBlockField string

const (
	BlockPrefix        RemoteIPBlockField = "prefix"
	BlockFailures      RemoteIPBlockField = "consecutive_failures"
	BlockLastFailureAt RemoteIPBlockField = "last_failure_at"
	BlockUntil         RemoteIPBlockField = "blocked_until"
	BlockUnblockedBy   RemoteIPBlockField = "unblocked_by"
	BlockUnblockedAt   RemoteIPBlockField = "unblocked_at"
)

func newRemoteIPBlocks(conn dao.DataConn) *dao.Schema[*RemoteIPBlock, RemoteIPBlockField, Sort, string] {
	return dao.New(conn,
		dao.Table[*RemoteIPBlock, RemoteIPBlockField, Sort, string]("remote_ip_blocks"),
		dao.ID[*RemoteIPBlock, RemoteIPBlockField, Sort, string](BlockPrefix),
		dao.Conflict[*RemoteIPBlock, RemoteIPBlockField, Sort, string](BlockPrefix),
		dao.Fields[*RemoteIPBlock, RemoteIPBlockField, Sort, string](map[RemoteIPBlockField]dao.Field[*RemoteIPBlock]{
			BlockPrefix:        {Column: "prefix", Scan: func(r *RemoteIPBlock) any { return &r.Prefix }, Value: func(r *RemoteIPBlock) any { return r.Prefix }},
			BlockFailures:      {Column: "consecutive_failures", Scan: func(r *RemoteIPBlock) any { return &r.ConsecutiveFailures }, Value: func(r *RemoteIPBlock) any { return r.ConsecutiveFailures }},
			BlockLastFailureAt: {Column: "last_failure_at", Scan: func(r *RemoteIPBlock) any { return &r.LastFailureAt }, Value: func(r *RemoteIPBlock) any { return r.LastFailureAt }},
			BlockUntil:         {Column: "blocked_until", Scan: func(r *RemoteIPBlock) any { return &r.BlockedUntil }, Value: func(r *RemoteIPBlock) any { return r.BlockedUntil }},
			BlockUnblockedBy:   {Column: "unblocked_by", Scan: func(r *RemoteIPBlock) any { return &r.UnblockedBy }, Value: func(r *RemoteIPBlock) any { return r.UnblockedBy }},
			BlockUnblockedAt:   {Column: "unblocked_at", Scan: func(r *RemoteIPBlock) any { return &r.UnblockedAt }, Value: func(r *RemoteIPBlock) any { return r.UnblockedAt }},
		}),
	)
}

// --- remote_denial_events -------------------------------------------------------

// RemoteDenial is the durable claim of one remote denial, keyed by the event id
// minted when it happened: a replay after a crash finds the claim and steps no
// counter twice.
type RemoteDenial struct {
	EventID      string
	Prefix       string
	OccurredAt   int64
	Reason       string
	OfferedKeyFP string
	UserID       int64
	AuditID      int64
}

type RemoteDenialField string

const (
	DenialEventID      RemoteDenialField = "event_id"
	DenialPrefix       RemoteDenialField = "prefix"
	DenialOccurredAt   RemoteDenialField = "occurred_at"
	DenialReason       RemoteDenialField = "reason"
	DenialOfferedKeyFP RemoteDenialField = "offered_key_fp"
	DenialUserID       RemoteDenialField = "user_id"
	DenialAuditID      RemoteDenialField = "audit_id"
)

func newRemoteDenials(conn dao.DataConn) *dao.Schema[*RemoteDenial, RemoteDenialField, Sort, string] {
	return dao.New(conn,
		dao.Table[*RemoteDenial, RemoteDenialField, Sort, string]("remote_denial_events"),
		dao.ID[*RemoteDenial, RemoteDenialField, Sort, string](DenialEventID),
		dao.Conflict[*RemoteDenial, RemoteDenialField, Sort, string](DenialEventID),
		dao.Fields[*RemoteDenial, RemoteDenialField, Sort, string](map[RemoteDenialField]dao.Field[*RemoteDenial]{
			DenialEventID:      {Column: "event_id", Scan: func(r *RemoteDenial) any { return &r.EventID }, Value: func(r *RemoteDenial) any { return r.EventID }},
			DenialPrefix:       {Column: "prefix", Scan: func(r *RemoteDenial) any { return &r.Prefix }, Value: func(r *RemoteDenial) any { return r.Prefix }},
			DenialOccurredAt:   {Column: "occurred_at", Scan: func(r *RemoteDenial) any { return &r.OccurredAt }, Value: func(r *RemoteDenial) any { return r.OccurredAt }},
			DenialReason:       {Column: "reason", Scan: func(r *RemoteDenial) any { return &r.Reason }, Value: func(r *RemoteDenial) any { return r.Reason }},
			DenialOfferedKeyFP: {Column: "offered_key_fp", Scan: func(r *RemoteDenial) any { return &r.OfferedKeyFP }, Value: func(r *RemoteDenial) any { return r.OfferedKeyFP }},
			DenialUserID:       {Column: "user_id", Scan: func(r *RemoteDenial) any { return &r.UserID }, Value: func(r *RemoteDenial) any { return r.UserID }},
			DenialAuditID:      {Column: "audit_id", Scan: func(r *RemoteDenial) any { return &r.AuditID }, Value: func(r *RemoteDenial) any { return r.AuditID }},
		}),
	)
}
