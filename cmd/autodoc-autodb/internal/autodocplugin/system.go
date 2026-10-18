package autodocplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// SYSTEM — what the server and this plugin are, and their repair. A server of another protocol is
// found by a probe (sys.hello with no protocol, its own connection), so the mismatch never poisons
// a working connection. Its repair is local only: an older server is restarted onto the release
// matching this plugin; an older plugin asks AutoDoc to update it (plugins.update, itself only).

// pluginName is the plugin's name in its manifest: plugins.update may name only itself.
const pluginName = "autodb"

// restartTimeout bounds the wait for a server that said it is stopping to let its connection go.
//
// NOTE: a deadline: the stopping server's connection ending is the event waited on, and a server
// that never lets go announces nothing else.
const restartTimeout = 30 * time.Second

// gap is how the plugin's protocol and the server's stand.
type gap int

const (
	even         gap = iota // the plugin's protocol: nothing to do
	serverBehind            // the server's protocol is older than the plugin's: restart it onto the plugin's release
	pluginBehind            // the server's is newer: update the plugin
)

func gapOf(pluginProtocol, serverProtocol int) gap {
	switch {
	case serverProtocol < pluginProtocol:
		return serverBehind
	case serverProtocol > pluginProtocol:
		return pluginBehind
	}
	return even
}

// mismatch is a server of another protocol, as its probe said.
type mismatch struct {
	network, addr string
	hello         autodb.Hello
	protocol      int
	gap           gap
}

// mismatched follows a hello the server refused for its protocol: the probe says what it is, and
// SYSTEM says what to do.
func (p *Plugin) mismatched(me *autodb.MismatchError) {
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	h, proto, err := autodb.Probe(ctx, me.Network, me.Addr)
	if err != nil {
		p.step, p.problem = stepFailed, me.Error()
		p.draw()
		return
	}
	p.mis = &mismatch{network: me.Network, addr: me.Addr, hello: h, protocol: proto, gap: gapOf(int(autodb.Protocol), proto)}
	p.step, p.problem, p.tab = stepMismatch, "", panel.TabSystem
	p.goTab = true
	p.draw()
}

// mismatchText is the mismatch step's line.
func (p *Plugin) mismatchText() string {
	m := p.mis
	if m == nil {
		return ""
	}
	switch m.gap {
	case serverBehind:
		return fmt.Sprintf("AutoDB %s speaks protocol %d, older than this plugin's %d: Menu › Upgrade AutoDB restarts it onto %s",
			m.hello.Version, m.protocol, autodb.Protocol, p.version)
	case pluginBehind:
		return fmt.Sprintf("AutoDB %s speaks protocol %d, newer than this plugin's %d: Menu › Update the plugin",
			m.hello.Version, m.protocol, autodb.Protocol)
	}
	return ""
}

// systemText is the SYSTEM tab: the server and this plugin.
func (p *Plugin) systemText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plugin  %s (protocol %d)\n", p.version, autodb.Protocol)
	if p.bin.Path != "" {
		fmt.Fprintf(&b, "binary  %s %s (%s)\n", p.bin.Path, p.bin.Version, p.bin.Via)
	}
	switch {
	case p.mis != nil && p.cli == nil:
		fmt.Fprintf(&b, "server  %s (protocol %d) at %s\nstore   %s\n\n%s", p.mis.hello.Version, p.mis.protocol,
			p.mis.addr, p.mis.hello.StoreID, p.mismatchText())
	case p.cli == nil:
		b.WriteString("server  not reached")
	default:
		fmt.Fprintf(&b, "server  %s at %s\nstore   %s", p.cli.Hello.Version, p.cli.Addr, p.cli.Hello.StoreID)
		if p.who.Name != "" {
			fmt.Fprintf(&b, "\nuser    %s (%s)", p.who.Name, p.who.Role)
		}
	}
	return b.String()
}

// systemRows are SYSTEM's menu rows.
func (p *Plugin) systemRows() []panel.MenuRow {
	signedIn := p.cli != nil && p.token != ""
	local := p.remote.on == nil // a remote server is its administrator's to restart
	m := p.mis
	return []panel.MenuRow{
		{ID: "sys.restart", Label: "&Restart server…", Enabled: signedIn && local},
		{ID: "sys.upgrade", Label: "&Upgrade AutoDB…", Enabled: p.step == stepMismatch && m != nil && m.gap == serverBehind},
		{ID: "sys.update", Label: "Update the &plugin…", Enabled: p.step == stepMismatch && m != nil && m.gap == pluginBehind},
		{ID: "auth.signout", Label: "Sign &out", Enabled: signedIn},
		{ID: "sys.retry", Label: "Re&try", Enabled: p.step == stepFailed || p.step == stepMismatch},
	}
}

// systemRow runs a SYSTEM row; false for one it does not know.
func (p *Plugin) systemRow(id string) bool {
	switch id {
	case "sys.restart":
		p.askConfirm("Restart AutoDB", "Restart the AutoDB server? It restarts only when nothing runs on it.", p.restart)
	case "sys.upgrade":
		if m := p.mis; m != nil {
			p.askConfirm("Upgrade AutoDB", fmt.Sprintf("Install AutoDB %s and restart the server (%s) onto it once nothing runs?",
				p.version, m.hello.Version), p.upgradeServer)
		}
	case "sys.update":
		p.askConfirm("Update the plugin", "Ask AutoDoc to update this plugin to the server's protocol?", p.upgradePlugin)
	case "auth.signout":
		p.signOut()
	default:
		return false
	}
	return true
}

// restart is SYSTEM › Restart: sys.restart_if_idle on the working connection; stopped, the server
// is started again and the sign-in resumed (a store with no service keyslot asks the passphrase,
// the name filled in).
func (p *Plugin) restart() {
	if p.cli == nil || p.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	r, err := p.cli.RestartIfIdle(ctx, p.token)
	if !p.restarting(r, err, p.cli.Hello.Version) {
		return
	}
	p.forgetTable(errors.New("AutoDB restarted"))
	p.waitGone(p.cli.Done())
	_ = p.cli.Close()
	p.cli, p.token, p.sid, p.conn, p.who = nil, "", "", nil, autodb.Who{}
	p.status = "AutoDB restarted"
	p.connect()
}

// restarting reads sys.restart_if_idle's answer, saying on the panel why it did not stop; it
// reports whether the server is stopping.
func (p *Plugin) restarting(r autodb.Restart, err error, version string) bool {
	switch {
	case err == nil && r.Stopping:
		return true
	case err == nil:
		p.problem = "AutoDB keeps running: " + r.Busy() + ". Try again when it is idle"
	case autodb.IsUnknownMethod(err):
		p.problem = "AutoDB " + version + " must be restarted by hand (autodb --serve stop, then start it)"
	case autodb.IsCode(err, autodb.CodeDenied):
		p.problem = "an administrator must restart AutoDB: nothing was changed"
	default:
		p.problem = "restarting AutoDB: " + autodb.Message(err)
	}
	p.status = ""
	p.draw()
	return false
}

// waitGone waits for a stopping server's connection to end, within restartTimeout.
func (p *Plugin) waitGone(done <-chan struct{}) {
	t := time.NewTimer(restartTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-p.ctx.Done():
	}
}

// upgradeServer is SYSTEM › Upgrade AutoDB (the server's protocol older): install the release
// matching the plugin into its bin/ (a failure changes nothing), then, on a lifecycle connection
// signed in with this store's record or the passphrase, ask the server to restart once idle, and
// start the plugin's release. If that fails to start, the previous binary is started again.
func (p *Plugin) upgradeServer() {
	m := p.mis
	if m == nil || m.gap != serverBehind {
		return
	}
	if !autodb.IsRelease(p.version) {
		p.problem = fmt.Sprintf("this plugin was built from a commit with no release (%s): build AutoDB here "+
			"(`go build -o bin/autodb ./cmd/autodb`), stop the running one, then Menu › Retry", p.version)
		p.draw()
		return
	}
	p.problem, p.status = "", "downloading AutoDB "+p.version+"…"
	p.draw()
	ictx, icancel := context.WithTimeout(p.ctx, installTimeout)
	err := p.install(ictx)
	icancel()
	if err != nil {
		p.problem, p.status = "the upgrade changed nothing: "+err.Error(), ""
		p.draw()
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	lc, err := p.dialLifecycle(ctx, m)
	if err != nil {
		p.problem, p.status = "reaching AutoDB "+m.hello.Version+" to restart it: "+err.Error(), ""
		p.draw()
		return
	}
	if r, ok := p.creds[localTargetOf(m.hello.StoreID)]; ok {
		if w, err := lc.Whoami(ctx, r.Token); err == nil && w.Name != "" {
			p.restartOnto(lc, r.Token)
			return
		}
	}
	p.status = ""
	p.onSignIn = func(name, pass string) {
		lctx, lcancel := context.WithTimeout(p.ctx, loginTimeout)
		defer lcancel()
		token, err := lc.Login(lctx, strings.TrimSpace(name), pass)
		if err != nil {
			_ = lc.Close()
			p.problem = "sign-in refused: " + autodb.Message(err) + ". Menu › Upgrade AutoDB to try again"
			p.draw()
			return
		}
		p.restartOnto(lc, token)
	}
	names, _ := readLogins(p.logins)
	p.openDialog(panel.DialogSignIn, map[string]any{"signInName": names["store:"+m.hello.StoreID],
		"signInNote": "AutoDB " + m.hello.Version + " restarts onto " + p.version + ": an administrator's sign-in"})
}

// restartOnto asks the server on lc to restart once idle; stopping, the plugin's release starts.
func (p *Plugin) restartOnto(lc *autodb.Lifecycle, token string) {
	defer lc.Close()
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	r, err := lc.RestartIfIdle(ctx, token)
	cancel()
	if !p.restarting(r, err, lc.Hello.Version) {
		return
	}
	p.waitGone(lc.Done())
	prev := p.bin
	p.bin = autodb.Binary{Path: autodb.InstallPath(p.pluginDir), Version: p.version, Via: "this plugin's bin/"}
	p.mis = nil
	p.connect()
	if p.step == stepFailed && prev.Path != "" && prev.Path != p.bin.Path {
		why := p.problem
		p.bin = prev
		p.connect()
		p.problem = "AutoDB " + p.version + " did not start (" + why + "): the previous one was started again"
		p.draw()
	}
}

// dialLifecycle connects to the mismatched server at its own protocol.
func (p *Plugin) dialLifecycle(ctx context.Context, m *mismatch) (*autodb.Lifecycle, error) {
	if p.lifecycle != nil {
		return p.lifecycle(ctx, m.network, m.addr, m.protocol)
	}
	return autodb.DialLifecycle(ctx, m.network, m.addr, p.version, m.protocol)
}

// upgradePlugin is SYSTEM › Update the plugin (the server's protocol newer): AutoDoc updates the
// plugin by its own name, then starts it again (plugin.close "update": the slot keeps the
// records). A refused update leaves the plugin as it was, saying why.
func (p *Plugin) upgradePlugin() {
	if p.host == nil {
		return
	}
	ok, err := p.host.Call("plugins.update", pluginName)
	var ce *plugin.CallError
	switch {
	case err == nil && ok:
		p.status, p.problem = "AutoDoc is updating the plugin…", ""
	case err == nil:
		p.problem = "the update was not made"
	case errors.As(err, &ce) && ce.Code == plugin.CallUnknown, errors.Is(err, plugin.ErrNoCalls):
		p.problem = "this AutoDoc can't update a plugin itself: Plugins › Manage › Update"
	case errors.As(err, &ce):
		p.problem = "the update failed: " + ce.Message
	default:
		p.problem = "the update failed: " + err.Error()
	}
	p.draw()
}

// signOut is SYSTEM › Sign out: the session ends and its record goes.
func (p *Plugin) signOut() {
	if p.cli == nil || p.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, controlTimeout)
	defer cancel()
	p.forgetTable(errors.New("signed out of AutoDB"))
	_ = p.cli.Logout(ctx, p.token)
	p.forget()
	p.token, p.sid, p.conn, p.who = "", "", nil, autodb.Who{}
	p.step, p.status, p.problem = stepLogin, "signed out", ""
	p.draw()
}
