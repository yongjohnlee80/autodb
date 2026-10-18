package autodb

import (
	"context"
	"errors"
)

// THE PANEL'S VERBS (ADR 1792301900): who is signed in, the first run, workspaces and connections,
// and notes. Each takes the session's token first, as the server's handlers do.

// Who is auth.whoami's answer.
type Who struct {
	ID   int64
	Name string
	Role string
	// StoreUnlocked is whether the store's master key is in memory; an older server does not say
	// (Known false), and the first ErrLocked tells instead.
	StoreUnlocked, Known bool
}

// Whoami is who the token signs in as, and whether the store is unlocked.
func (c *Client) Whoami(ctx context.Context, token string) (Who, error) {
	return whoami(ctx, c.call, token)
}

// Bootstrap makes the store's first user, an administrator, and returns its session's token.
func (c *Client) Bootstrap(ctx context.Context, name, passphrase string) (string, error) {
	res, err := c.call(ctx, "auth.bootstrap", name, passphrase)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	token, _ := m["token"].(string)
	if token == "" {
		return "", errors.New("auth.bootstrap answered without a token")
	}
	return token, nil
}

// Workspace is a workspace and the connections in it.
type Workspace struct {
	ID          int64
	Name        string
	Connections []Connection
}

// Workspaces are the workspaces the token may see, with their connections.
func (c *Client) Workspaces(ctx context.Context, token string) ([]Workspace, error) {
	res, err := c.call(ctx, "workspace.list", token)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]Workspace, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		w := Workspace{}
		w.ID, _ = intOf(m["id"])
		w.Name, _ = m["name"].(string)
		conns, _ := m["connections"].([]any)
		for _, cv := range conns {
			cm, _ := cv.(map[string]any)
			conn := Connection{Workspace: w.Name}
			conn.ID, _ = intOf(cm["id"])
			conn.Name, _ = cm["name"].(string)
			conn.Engine, _ = cm["engine"].(string)
			w.Connections = append(w.Connections, conn)
		}
		out = append(out, w)
	}
	return out, nil
}

// CreateWorkspace makes a workspace and returns its id.
func (c *Client) CreateWorkspace(ctx context.Context, token, name string) (int64, error) {
	res, err := c.call(ctx, "workspace.create", token, name)
	if err != nil {
		return 0, err
	}
	id, _ := intOf(res)
	return id, nil
}

// RenameWorkspace relabels a workspace.
func (c *Client) RenameWorkspace(ctx context.Context, token string, ws int64, name string) error {
	_, err := c.call(ctx, "workspace.rename", token, ws, name)
	return err
}

// DeleteWorkspace deletes a workspace; the server refuses one that holds connections.
func (c *Client) DeleteWorkspace(ctx context.Context, token string, ws int64) error {
	_, err := c.call(ctx, "workspace.delete", token, ws)
	return err
}

// Attach puts connection conn in workspace ws.
func (c *Client) Attach(ctx context.Context, token string, ws, conn int64) error {
	_, err := c.call(ctx, "workspace.attach", token, ws, conn)
	return err
}

// CreateConnection makes a connection (engine postgres, mysql or sqlite) and returns its id.
func (c *Client) CreateConnection(ctx context.Context, token, name, engine, dsn string) (int64, error) {
	res, err := c.call(ctx, "conn.create", token, name, engine, dsn)
	if err != nil {
		return 0, err
	}
	id, _ := intOf(res)
	return id, nil
}

// TestConnection dials the connection's database; nil when it answered.
func (c *Client) TestConnection(ctx context.Context, token string, conn int64) error {
	_, err := c.call(ctx, "conn.test", token, conn)
	return err
}

// RenameConnection relabels a connection.
func (c *Client) RenameConnection(ctx context.Context, token string, conn int64, name string) error {
	_, err := c.call(ctx, "conn.rename", token, conn, name)
	return err
}

// DeleteConnection deletes a connection, or archives one with history, and says which.
func (c *Client) DeleteConnection(ctx context.Context, token string, conn int64) (archived bool, err error) {
	res, err := c.call(ctx, "conn.delete", token, conn)
	if err != nil {
		return false, err
	}
	m, _ := res.(map[string]any)
	archived, _ = m["archived"].(bool)
	return archived, nil
}

// Notes are a workspace's notes, by name.
func (c *Client) Notes(ctx context.Context, token string, ws int64) ([]string, error) {
	res, err := c.call(ctx, "notes.list", token, ws)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// ReadNote is a note's text and version: an opaque string, kept exactly as given ("" for a note
// not yet written).
func (c *Client) ReadNote(ctx context.Context, token string, ws int64, name string) (body, version string, err error) {
	res, err := c.call(ctx, "notes.read", token, ws, name)
	if err != nil {
		return "", "", err
	}
	m, _ := res.(map[string]any)
	body, _ = m["body"].(string)
	version, _ = m["version"].(string)
	return body, version, nil
}

// CreateNote makes an empty note and returns its version.
func (c *Client) CreateNote(ctx context.Context, token string, ws int64, name string) (string, error) {
	res, err := c.call(ctx, "notes.create", token, ws, name)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	version, _ := m["version"].(string)
	return version, nil
}

// WriteNote writes a note if its version on the server is still expected (CodeNoteConflict when
// it is not), and returns the new version.
func (c *Client) WriteNote(ctx context.Context, token string, ws int64, name, body, expected string) (string, error) {
	res, err := c.call(ctx, "notes.write", token, ws, name, body, expected)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	version, _ := m["version"].(string)
	return version, nil
}

// DeleteNote deletes a note.
func (c *Client) DeleteNote(ctx context.Context, token string, ws int64, name string) error {
	_, err := c.call(ctx, "notes.delete", token, ws, name)
	return err
}
