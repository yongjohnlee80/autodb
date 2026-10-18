package autodocplugin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// THE SIGN-IN KEPT — AutoDoc keeps a plugin's session slot in its memory until it quits (plugin
// protocol 7), and hands it back at each start. The plugin keeps a record there per target, the
// local server by its store, so turning the card off and on, or updating the plugin, does not ask
// again: the record's token is checked with auth.whoami, which also says whether the store is
// unlocked (a server restarted with no service keyslot holds a valid token and a locked store: its
// passphrase is asked, the name filled in). The plugin logs out only when AutoDoc quits or removes
// it; a turned-off plugin keeps its sign-in for the next start.
//
//	"v"                   "1"                          the encoding's version
//	"t:local:<store id>"  {"user","token","at"}        the local server's store

// slotVersion is the encoding's version: a slot of another is ignored, not migrated.
const slotVersion = "1"

// maxTargets is the records kept; past it the oldest goes.
const maxTargets = 8

// credRecord is a target's sign-in.
type credRecord struct {
	User  string `json:"user"`
	Token string `json:"token"`
	// Device is a remote record's device key fingerprint: it is resumed only with that key
	Device string `json:"device,omitempty"`
	At     int64  `json:"at"` // unix seconds, for the oldest to go first
}

// creds are the slot's records, by target key.
type creds map[string]credRecord

// readCreds is the slot as AutoDoc handed it back: a slot of another version, or a record that is
// not one, counts as none.
func readCreds(slot map[string]string) creds {
	out := creds{}
	if slot["v"] != slotVersion {
		return out
	}
	for k, v := range slot {
		if !strings.HasPrefix(k, "t:") {
			continue
		}
		var r credRecord
		if json.Unmarshal([]byte(v), &r) == nil && r.Token != "" {
			out[k] = r
		}
	}
	return out
}

// slot is the records as AutoDoc keeps them: the newest maxTargets.
func (c creds) slot() map[string]string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return int(c[b].At - c[a].At) })
	out := map[string]string{"v": slotVersion}
	for _, k := range keys[:min(len(keys), maxTargets)] {
		b, _ := json.Marshal(c[k])
		out[k] = string(b)
	}
	return out
}

// localTarget is the local server's key: its store, which a restart keeps.
func localTarget(cli *autodb.Client) string {
	if cli == nil {
		return ""
	}
	return localTargetOf(cli.Hello.StoreID)
}

// localTargetOf is the local target of the store storeID, "" for a server that names none.
func localTargetOf(storeID string) string {
	if storeID == "" {
		return ""
	}
	return "t:local:" + storeID
}

// saveCreds sends the records to AutoDoc's slot. A host before protocol 7 keeps none: the sign-in
// is asked at each start, as it always was.
func (p *Plugin) saveCreds() {
	if s, ok := p.host.(sessionHost); ok {
		_ = s.SetSession(p.creds.slot())
	}
}

// sessionHost is a host that keeps the slot (*plugin.Peer at protocol 7).
type sessionHost interface {
	SetSession(values map[string]string) error
	CloseReason() string
}

// keep records the sign-in to the server connected, and sends the slot.
func (p *Plugin) keep(user string) {
	key := p.targetKey()
	if key == "" {
		return // a server that names no store: no record can be matched to it again
	}
	r := credRecord{User: user, Token: p.token, At: time.Now().Unix()}
	if on := p.remote.on; on != nil {
		r.Device = deviceFP(p.remote.keys[on.prof.ID])
	}
	p.creds[key] = r
	p.saveCreds()
}

// forget drops the record of the server connected, and sends the slot.
func (p *Plugin) forget() {
	key := p.targetKey()
	if _, ok := p.creds[key]; !ok {
		return
	}
	delete(p.creds, key)
	p.saveCreds()
}

// resume follows connect: the record for this store, if any, is tried. Signed in (the token
// valid, the store unlocked): no form. The token valid but the store locked: the sign-in, the
// name filled in, saying so. The token refused: the record goes, and the sign-in opens. It reports
// whether it signed in.
func (p *Plugin) resume() bool {
	key := p.targetKey()
	r, ok := p.creds[key]
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	who, err := p.cli.Whoami(ctx, r.Token)
	if err != nil {
		if _, answered := autodb.Code(err); answered {
			p.forget() // expired, revoked, or the store not the one it was for
		}
		return false
	}
	if who.Known && !who.StoreUnlocked {
		p.resumeName = who.Name
		p.problem = "AutoDB restarted and its store is locked: your passphrase unlocks it"
		return false
	}
	p.token, p.who = r.Token, who
	p.status = "signed in as " + who.Name
	return true
}

// logoutOnClose reports whether closing logs the sign-in out: on quit or removal, the sign-in
// ending with AutoDoc, and with a host that gives no reason (before protocol 7, or no plugin.close
// came); not when the plugin is only turned off or updated, its record kept for the next start.
func (p *Plugin) logoutOnClose() bool {
	s, ok := p.host.(sessionHost)
	if !ok {
		return true
	}
	switch s.CloseReason() {
	case plugin.CloseOff, plugin.CloseUpdate:
		_, kept := p.creds[p.targetKey()] // the target connected: a remote server's record too
		return !kept
	}
	return true
}
