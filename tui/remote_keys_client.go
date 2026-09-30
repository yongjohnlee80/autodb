package tui

import "context"

// The SSH keys registered on autodb profiles and the devices enrolled with
// them, as remote.ssh_key_* and remote.device_* answer them. They are the
// connected server's: a local session administers its own daemon's keys, a
// remote one the remote server's.

// SSHKeyRow is one live SSH key and its live device, if it has one.
type SSHKeyRow struct {
	ID          int64
	UserID      int64
	User        string
	Label       string
	Fingerprint string
	Type        string
	CreatedAt   int64
	LastUsedAt  int64
	Device      *DeviceRow
}

// DeviceRow is one enrolled device.
type DeviceRow struct {
	ID          int64
	SSHKeyID    int64
	UserID      int64
	User        string
	Fingerprint string
	EnrolledAt  int64
	EnrolledIP  string
	LastSeenAt  int64
	LastIP      string
	RevokedAt   int64
}

func deviceRowFromWire(m map[string]any) DeviceRow {
	return DeviceRow{
		ID: mI(m, "id"), SSHKeyID: mI(m, "ssh_key_id"), UserID: mI(m, "user_id"), User: mS(m, "user"),
		Fingerprint: mS(m, "fingerprint"), EnrolledAt: mI(m, "enrolled_at"), EnrolledIP: mS(m, "enrolled_ip"),
		LastSeenAt: mI(m, "last_seen_at"), LastIP: mS(m, "last_ip"), RevokedAt: mI(m, "revoked_at"),
	}
}

func sshKeyRowFromWire(m map[string]any) SSHKeyRow {
	k := SSHKeyRow{
		ID: mI(m, "id"), UserID: mI(m, "user_id"), User: mS(m, "user"), Label: mS(m, "label"),
		Fingerprint: mS(m, "fingerprint"), Type: mS(m, "type"),
		CreatedAt: mI(m, "created_at"), LastUsedAt: mI(m, "last_used_at"),
	}
	if d, ok := m["device"].(map[string]any); ok {
		dev := deviceRowFromWire(d)
		k.Device = &dev
	}
	return k
}

// SSHKeys lists the live SSH keys of userID (0: the caller's; -1: everyone's,
// for an admin), each with its live device.
func (b *Bound) SSHKeys(ctx context.Context, userID int64) ([]SSHKeyRow, error) {
	res, err := b.authed(ctx, "remote.ssh_key_list", userID)
	if err != nil {
		return nil, err
	}
	var out []SSHKeyRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, sshKeyRowFromWire(m))
	}
	return out, nil
}

// AddSSHKey registers a public key (an authorized_keys line) on userID's
// profile (0: the caller's). Adding to one's own profile needs the
// passphrase, and a wrong one is a failed sign-in; an admin adding to
// another's passes "".
func (b *Bound) AddSSHKey(ctx context.Context, userID int64, publicKey, label, passphrase string) (SSHKeyRow, error) {
	res, err := b.authed(ctx, "remote.ssh_key_add", userID, publicKey, label, passphrase)
	if err != nil {
		return SSHKeyRow{}, err
	}
	m, _ := res.(map[string]any)
	return sshKeyRowFromWire(m), nil
}

// LabelSSHKey renames a key.
func (b *Bound) LabelSSHKey(ctx context.Context, keyID int64, label string) error {
	_, err := b.authed(ctx, "remote.ssh_key_label", keyID, label)
	return err
}

// RevokeSSHKey revokes a key, its device and their sessions; the server ends
// their live connections.
func (b *Bound) RevokeSSHKey(ctx context.Context, keyID int64) error {
	_, err := b.authed(ctx, "remote.ssh_key_revoke", keyID)
	return err
}

// Devices lists the devices of userID (0: the caller's; -1: everyone's, for
// an admin), revoked ones too when withRevoked.
func (b *Bound) Devices(ctx context.Context, userID int64, withRevoked bool) ([]DeviceRow, error) {
	res, err := b.authed(ctx, "remote.device_list", userID, withRevoked)
	if err != nil {
		return nil, err
	}
	var out []DeviceRow
	for _, row := range asList(res) {
		m, _ := row.(map[string]any)
		out = append(out, deviceRowFromWire(m))
	}
	return out, nil
}

// RevokeDevice revokes a device and its sessions; the server ends its live
// connections. Its SSH key stays, free for a new device's first connect.
func (b *Bound) RevokeDevice(ctx context.Context, deviceID int64) error {
	_, err := b.authed(ctx, "remote.device_revoke", deviceID)
	return err
}
