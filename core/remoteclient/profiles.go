package remoteclient

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile is one remote autodb server as the user reaches it: the
// mechanics of the connection and nothing else. No secret is kept in it.
type Profile struct {
	// ID names the profile and its device key file.
	ID   string `toml:"id"`
	Name string `toml:"name"`
	Host string `toml:"host"`
	Port int    `toml:"port"`
	// HostKeyFP is the server's pinned host key (SHA256:…); empty until the
	// first connect, where the user confirms it against the admin's copy.
	HostKeyFP string `toml:"host_key_fp"`
	// User is the autodb user signed in as on the server.
	User string `toml:"user"`
	// KeyFile is the SSH key to offer (default ~/.ssh/id_ed25519). With
	// UseAgent the agent signs for it, and only its public half
	// (KeyFile.pub) is read: the one key offered is still this one.
	KeyFile  string `toml:"key_file"`
	UseAgent bool   `toml:"use_agent"`
}

// DefaultPort is the remote listener's default port.
const DefaultPort = 7422

// DefaultKeyFile is the SSH key a new profile offers.
const DefaultKeyFile = "~/.ssh/id_ed25519"

var (
	// ErrProfileInvalid: a profile the TUI cannot connect with.
	ErrProfileInvalid = errors.New("remoteclient: invalid remote profile")
	idPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

func validID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: id %q must be lowercase letters, digits, - or _, at most 64", ErrProfileInvalid, id)
	}
	return nil
}

// Validate checks p, and fills the port and key defaults.
func (p *Profile) Validate() error {
	if err := validID(p.ID); err != nil {
		return err
	}
	p.Host = strings.TrimSpace(p.Host)
	if p.Host == "" || strings.ContainsAny(p.Host, " /") {
		return fmt.Errorf("%w: %s: host %q", ErrProfileInvalid, p.ID, p.Host)
	}
	if p.Port == 0 {
		p.Port = DefaultPort
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("%w: %s: port %d", ErrProfileInvalid, p.ID, p.Port)
	}
	if strings.TrimSpace(p.User) == "" {
		return fmt.Errorf("%w: %s: no autodb user", ErrProfileInvalid, p.ID)
	}
	if p.KeyFile == "" {
		p.KeyFile = DefaultKeyFile
	}
	if p.HostKeyFP != "" && !strings.HasPrefix(p.HostKeyFP, "SHA256:") {
		return fmt.Errorf("%w: %s: host_key_fp %q is not a SHA256 fingerprint", ErrProfileInvalid, p.ID, p.HostKeyFP)
	}
	return nil
}

// Address is host:port.
func (p Profile) Address() string { return fmt.Sprintf("%s:%d", p.Host, p.Port) }

type profilesFile struct {
	Remote []Profile `toml:"remote"`
}

// ProfilesPath is $XDG_CONFIG_HOME/autodb/remotes.toml.
func ProfilesPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autodb", "remotes.toml"), nil
}

// LoadProfiles reads the profiles at path; no file is no profiles. An
// unknown key, a duplicate id or an invalid profile refuses the file.
func LoadProfiles(path string) ([]Profile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f profilesFile
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrProfileInvalid, path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%w: %s: unknown key %s", ErrProfileInvalid, path, und[0])
	}
	seen := map[string]bool{}
	for i := range f.Remote {
		if err := f.Remote[i].Validate(); err != nil {
			return nil, err
		}
		if seen[f.Remote[i].ID] {
			return nil, fmt.Errorf("%w: %s: id %q twice", ErrProfileInvalid, path, f.Remote[i].ID)
		}
		seen[f.Remote[i].ID] = true
	}
	return f.Remote, nil
}

// SaveProfiles writes profiles to path atomically, 0600 in a 0700
// directory.
func SaveProfiles(path string, profiles []Profile) error {
	seen := map[string]bool{}
	for i := range profiles {
		if err := profiles[i].Validate(); err != nil {
			return err
		}
		if seen[profiles[i].ID] {
			return fmt.Errorf("%w: id %q twice", ErrProfileInvalid, profiles[i].ID)
		}
		seen[profiles[i].ID] = true
	}
	var buf bytes.Buffer
	buf.WriteString("# autodb remote profiles: how this machine reaches remote autodb servers.\n# No secrets are kept here.\n\n")
	if err := toml.NewEncoder(&buf).Encode(profilesFile{Remote: profiles}); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(dir, ".remotes.*.tmp", path, buf.Bytes())
}

// writeAtomic writes data to path through a 0600 temp file in dir: written,
// fsynced, renamed into place, and the directory fsynced.
func writeAtomic(dir, pattern, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
