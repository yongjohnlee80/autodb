package autodocplugin

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
)

// THE LAST SIGN-IN — the name each AutoDB server was last signed in to with, kept so the next
// sign-in to that server starts with it typed in.
//
// It is a JSON object, {server: name}, in $XDG_STATE_HOME/autodoc-autodb/logins.json
// (~/.local/state/autodoc-autodb/logins.json when XDG_STATE_HOME is unset): a name last used is
// state, not a setting, and AutoDB keeps its own state under the same root. The plugin's directory
// is its checkout, rebuilt when AutoDoc updates it, so nothing is kept there.
//
// The file holds names only: never a passphrase, a token or anything else secret. It is written
// whole to a temporary file beside it that is then renamed over it, mode 0600, so a reader sees
// the old record or the new one and never half of either.

// loginsFile is the record's name under the state directory.
const loginsFile = "autodoc-autodb/logins.json"

// loginsPath is where the record lives, "" when there is no home to keep it in.
func loginsPath(getenv func(string) string) string {
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, filepath.FromSlash(loginsFile))
}

// serverKey names the server c reached in the record: the meta store it serves, where its users
// live, when it reports one, which stays the same across restarts and a changed endpoint; else the
// address it was reached at.
func serverKey(c *autodb.Client) string {
	if c.Hello.StoreID != "" {
		return "store:" + c.Hello.StoreID
	}
	return "addr:" + c.Addr
}

// readLogins is the record at path: empty when there is none (or no path), an error when the file
// cannot be read or is not a JSON object of names.
func readLogins(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return map[string]string{}, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]string{}, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// rememberLogin records name as the last signed in to server, keeping the other servers' names.
// A record that could not be read is replaced.
func rememberLogin(path, server, name string) error {
	if path == "" {
		return nil
	}
	m, _ := readLogins(path)
	if m[server] == name {
		return nil
	}
	m[server] = name
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// writeAtomic writes b to path through a temporary file in the same directory, renamed over path
// once it is whole and synced; the file is mode 0600 and its directory 0700.
func writeAtomic(path string, b []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".logins-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
