package autodocplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// REMOTE NOTES — a remote server's note opens in AutoDoc as a cache file, its working copy, beside
// a sidecar <name>.sql.autodb that binds it to its note: the target, the user it is the note of,
// the workspace, the name, and the server's version it was last fetched or pushed at. The binding
// lives in the path and the sidecar, so it outlasts the plugin, the buffer and AutoDoc; :autodb
// save pushes the command's document to the note its sidecar names, and nothing else.
//
//	<cache root>/<cacheTarget>/u-<hex16(sha256(subject))>/ws-<id>/<name>
//
// The plugin writes a working copy only on its first fetch or the user's Yes, and then renames the
// old copy aside first: an edit is never the only copy lost.

// sidecarV is the sidecar's encoding.
const sidecarV = 1

// sidecar is <name>.sql.autodb: never a token.
type sidecar struct {
	V       int    `json:"v"`
	Target  string `json:"target"`
	Subject string `json:"subject"`
	WS      int64  `json:"ws"`
	Name    string `json:"name"`
	// Base is the server's version last fetched or pushed, opaque, exactly as the server gave it
	Base     string `json:"base"`
	BaseHash string `json:"base_hash"`
	// Server is the server's version a conflict wrote to <name>.server, "" for none
	Server string `json:"server,omitempty"`
}

func hex16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cacheTarget names a remote server's notes on this machine: the profile's name and every field
// its connection uses, the device key and the store. An edited profile, even under its old name, is
// a new target, and the old files no longer any connected target's.
func cacheTarget(prof remoteclient.Profile, device, storeID string) string {
	return hex16(strings.Join([]string{prof.ID, prof.Name, prof.Address(), prof.User, prof.HostKeyFP,
		prof.KeyFile, fmt.Sprint(prof.UseAgent), device, storeID}, "\x00"))
}

// cachePath is a remote note's working copy under root.
func cachePath(root, target, subject string, ws int64, name string) string {
	return filepath.Join(root, target, "u-"+hex16(subject), fmt.Sprintf("ws-%d", ws), name)
}

// cacheRoot is where the working copies are: $XDG_CACHE_HOME/autodoc-autodb/notes.
func (p *Plugin) cacheRoot() (string, error) {
	if p.cacheDir != "" {
		return p.cacheDir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autodoc-autodb", "notes"), nil
}

// currentTarget is the remote server connected's cacheTarget, "" while local.
func (p *Plugin) currentTarget() string {
	on := p.remote.on
	if on == nil || p.cli == nil {
		return ""
	}
	return cacheTarget(on.prof, deviceFP(p.remote.keys[on.prof.ID]), p.cli.Hello.StoreID)
}

func sidecarPath(path string) string { return path + ".autodb" }
func serverPath(path string) string  { return path + ".server" }

// versionSafe is a version as a file name's part.
var versionUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func asidePath(path, base string) string {
	v := versionUnsafe.ReplaceAllString(base, "")
	return path + ".~v" + v[:min(len(v), 16)]
}

// writeFile writes b at path through a temporary file and a rename, its directories 0700.
func writeFile(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// writeSidecar writes path's sidecar; a failure is kept in memory and retried at the next save or
// open of path.
func (p *Plugin) writeSidecar(path string, sc sidecar) error {
	b, _ := json.Marshal(sc)
	write := writeFile
	if p.sidecarWrite != nil {
		write = p.sidecarWrite
	}
	if err := write(sidecarPath(path), b, 0o600); err != nil {
		if p.pendingSidecars == nil {
			p.pendingSidecars = map[string]sidecar{}
		}
		p.pendingSidecars[path] = sc
		return err
	}
	delete(p.pendingSidecars, path)
	return nil
}

// sidecarOf reads path's sidecar: the one waiting to be written first, else the file's.
func (p *Plugin) sidecarOf(path string) (sidecar, error) {
	if sc, ok := p.pendingSidecars[path]; ok {
		_ = p.writeSidecar(path, sc) // a retry: still failing keeps it pending
		return sc, nil
	}
	var sc sidecar
	b, err := os.ReadFile(sidecarPath(path))
	if err == nil {
		err = json.Unmarshal(b, &sc)
	}
	if err == nil && sc.V != sidecarV {
		err = fmt.Errorf("a sidecar of version %d", sc.V)
	}
	return sc, err
}

// openRemoteNote opens a remote server's note as its working copy: fetched if there is none; a
// clean copy of an older version asks whether to take the server's; an edited copy is opened
// untouched, the server's newer text written beside it.
func (p *Plugin) openRemoteNote(ws int64, name string) {
	root, err := p.cacheRoot()
	if err != nil || p.who.Name == "" {
		p.problem = "the note's working copy has no place: " + fmt.Sprint(err)
		p.draw()
		return
	}
	target := p.currentTarget()
	path := cachePath(root, target, p.who.Name, ws, name)
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	body, ver, err := p.cli.ReadNote(ctx, p.token, ws, name)
	if err != nil {
		p.failed("reading "+name, err)
		p.draw()
		return
	}
	fresh := sidecar{V: sidecarV, Target: target, Subject: p.who.Name, WS: ws, Name: name, Base: ver, BaseHash: textHash(body)}
	if why := cachePlace(root, path); why != "" {
		p.problem = "opening " + name + ": " + why
		p.draw()
		return
	}
	working, rerr := os.ReadFile(path)
	sc, serr := p.sidecarOf(path)
	switch {
	case errors.Is(rerr, fs.ErrNotExist):
		if err := writeFile(path, []byte(body), 0o600); err != nil {
			p.problem = "writing " + name + "'s working copy: " + err.Error()
			p.draw()
			return
		}
		_ = p.writeSidecar(path, fresh)
	case rerr != nil:
		p.problem = "reading " + name + "'s working copy: " + rerr.Error()
		p.draw()
		return
	case serr != nil || textHash(string(working)) != sc.BaseHash: // edited, or bound to nothing known
		if serr != nil {
			sc = fresh
			sc.Base, sc.BaseHash = "", ""
			if string(working) == body {
				sc = fresh
			}
		}
		if sc.Base != ver {
			p.conflict(path, sc, body, ver)
			p.callOpen(path, name)
			return
		}
		if serr != nil {
			_ = p.writeSidecar(path, sc)
		}
	case sc.Base != ver: // clean, and the server moved
		p.askConfirm("The server's copy is newer", name+" changed on the server since it was fetched. Use the server's copy? "+
			"(No opens yours; Yes keeps yours as "+filepath.Base(asidePath(path, sc.Base))+")", func() {
			if err := os.Rename(path, asidePath(path, sc.Base)); err == nil || errors.Is(err, fs.ErrNotExist) {
				if err := writeFile(path, []byte(body), 0o600); err != nil {
					p.problem = "writing " + name + ": " + err.Error()
				} else {
					_ = p.writeSidecar(path, fresh)
				}
			}
			p.callOpen(path, name)
		})
		p.callOpen(path, name)
		return
	}
	p.callOpen(path, name)
}

// plainFile says why rel, under r, is not a plain file of the cache ("" when it is): a link, even
// to a file inside, is refused, so the note's bytes are never written anywhere but the cache.
func plainFile(r *os.Root, rel string) string {
	fi, err := r.Lstat(rel)
	switch {
	case err != nil:
		return "its working copy cannot be read (" + err.Error() + ")"
	case !fi.Mode().IsRegular():
		return "its working copy is not a plain file of the cache (a link, or not a file)"
	}
	return ""
}

// cachePlace says why path may not be a working copy under root ("" when it may): none yet, in
// the cache's own directories, or a plain file there.
func cachePlace(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "its working copy would be outside the cache"
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err.Error()
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err.Error()
	}
	defer r.Close()
	if _, err := r.Lstat(rel); errors.Is(err, fs.ErrNotExist) {
		fi, derr := r.Lstat(filepath.Dir(rel))
		switch {
		case errors.Is(derr, fs.ErrNotExist): // the write makes it, under the root
			return ""
		case derr == nil && fi.IsDir():
			return ""
		}
		return "its working copy's directory is not one of the cache's own"
	}
	return plainFile(r, rel)
}

// conflict writes the server's text beside the working copy, read-only, and records its version.
func (p *Plugin) conflict(path string, sc sidecar, body, ver string) {
	_ = os.Chmod(serverPath(path), 0o600)
	if err := writeFile(serverPath(path), []byte(body), 0o400); err != nil {
		p.problem = "writing the server's copy: " + err.Error()
		return
	}
	sc.Server = ver
	_ = p.writeSidecar(path, sc)
	p.problem = "the server's copy of " + sc.Name + " changed: compare " + filepath.Base(serverPath(path)) +
		", then NOTES › Save over the server's"
}

// callOpen opens path in AutoDoc.
func (p *Plugin) callOpen(path, name string) {
	ok, err := p.host.Call("file.open_path", path)
	var ce *plugin.CallError
	switch {
	case err == nil && ok:
		if p.problem == "" {
			p.status = "opened " + name
		}
	case errors.As(err, &ce) && ce.Code == plugin.CallUnknown, errors.Is(err, plugin.ErrNoCalls):
		p.problem = "this AutoDoc can't open a file by its path: update AutoDoc"
	case errors.As(err, &ce):
		p.problem = "AutoDoc could not open " + name + ": " + ce.Message
	case err != nil:
		p.problem = "opening " + name + ": " + err.Error()
	}
	p.draw()
}

// saveNote pushes d, the document a command ran on, to the remote note its working copy is bound
// to: AutoDoc saves the buffer first (file.save), then notes.write expects the sidecar's version,
// or with over the server's version a conflict showed. Refused, nothing is written: a document
// that is not a working copy, one whose sidecar does not name its very place, another server's or
// another user's note.
func (p *Plugin) saveNote(d plugin.Document, over bool) {
	if p.remote.on == nil || p.cli == nil || p.token == "" {
		p.problem = "save: connect to the note's remote server first (a local note is AutoDoc's own file: its save is the note's)"
		p.draw()
		return
	}
	sc, path, why := p.boundNote(d)
	if why != "" {
		p.problem = "save: " + why + ": nothing was written"
		p.draw()
		return
	}
	expected := sc.Base
	if over {
		if sc.Server == "" {
			p.problem = "save over: no conflict to settle; :autodb save pushes it"
			p.draw()
			return
		}
		expected = sc.Server
	}
	if ok, err := p.host.Call("file.save"); err != nil || !ok {
		p.problem = fmt.Sprintf("save: AutoDoc did not save the buffer (%v): nothing was pushed", err)
		p.draw()
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	ver, err := p.cli.WriteNote(ctx, p.token, sc.WS, sc.Name, d.Text, expected)
	if autodb.IsCode(err, autodb.CodeNoteConflict) {
		body, sv, rerr := p.cli.ReadNote(ctx, p.token, sc.WS, sc.Name)
		if rerr != nil {
			p.failed("reading the server's "+sc.Name, rerr)
			p.draw()
			return
		}
		p.conflict(path, sc, body, sv)
		if body == d.Text {
			p.problem = "the server's copy of " + sc.Name + " equals yours: NOTES › Save over the server's settles it"
		}
		p.draw()
		return
	}
	if err != nil {
		p.failed("saving "+sc.Name, err)
		p.draw()
		return
	}
	sc.Base, sc.BaseHash, sc.Server = ver, textHash(d.Text), ""
	_ = os.Remove(serverPath(path))
	p.status, p.problem = "saved "+sc.Name+" to "+profileName(p.remote.on.prof), ""
	if err := p.writeSidecar(path, sc); err != nil {
		p.problem = "saved, but its sidecar could not be written (" + err.Error() + "): it is retried"
	}
	p.draw()
}

// boundNote is the sidecar of d's working copy, and why it may not be saved ("" when it may).
func (p *Plugin) boundNote(d plugin.Document) (sidecar, string, string) {
	root, err := p.cacheRoot()
	if err != nil {
		return sidecar{}, "", err.Error()
	}
	path := filepath.Clean(d.Path)
	rel, err := filepath.Rel(root, path)
	if d.Path == "" || !filepath.IsAbs(d.Path) || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return sidecar{}, "", "this buffer is not a remote note's working copy"
	}
	// os.Root confines the reads: a link out of the cache is refused, and the working copy must be
	// a plain file of the cache, not a link to one elsewhere that AutoDoc's save would write
	r, err := os.OpenRoot(root)
	if err != nil {
		return sidecar{}, "", err.Error()
	}
	defer r.Close()
	if why := plainFile(r, rel); why != "" {
		return sidecar{}, "", why
	}
	b, err := r.ReadFile(rel + ".autodb")
	var sc sidecar
	if err == nil {
		err = json.Unmarshal(b, &sc)
	}
	if pending, ok := p.pendingSidecars[path]; ok {
		sc, err = pending, nil
	}
	switch {
	case err != nil:
		return sc, path, "its sidecar cannot be read (" + err.Error() + ")"
	case cachePath(root, sc.Target, sc.Subject, sc.WS, sc.Name) != path:
		return sc, path, "its sidecar names another place"
	case sc.Target != p.currentTarget():
		return sc, path, "it is a note of another server (or of this one's profile before it was edited)"
	case sc.Subject != p.who.Name:
		return sc, path, "it is " + sc.Subject + "'s note on " + profileName(p.remote.on.prof) + "; sign in as " + sc.Subject + " to save it"
	}
	return sc, path, ""
}

// deleteRemoteCopy removes a deleted note's working copy, only when it equals its base.
func (p *Plugin) deleteRemoteCopy(ws int64, name string) {
	root, err := p.cacheRoot()
	if err != nil || p.remote.on == nil {
		return
	}
	path := cachePath(root, p.currentTarget(), p.who.Name, ws, name)
	working, err := os.ReadFile(path)
	if err != nil {
		return
	}
	sc, serr := p.sidecarOf(path)
	if serr != nil || textHash(string(working)) != sc.BaseHash {
		p.problem = "its working copy has edits: kept at " + path
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(sidecarPath(path))
	_ = os.Remove(serverPath(path))
}
