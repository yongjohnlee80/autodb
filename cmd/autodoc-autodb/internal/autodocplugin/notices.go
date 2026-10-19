package autodocplugin

import (
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodoc/plugin"
)

// NOTICES — what the plugin has to say goes to AutoDoc's notifications (protocol 8), a toast
// wherever the user is, kept in the history: the card's own lines sat under its tabs and were lost
// with it hidden. A step's handler sets status and problem as it goes;
// when it returns (say) the last of each is sent once. A step that waits on AutoDB (a run, a script,
// a sign-in, a connect) says so at once with an ongoing notification, which what the step has to
// say then ends.

// progress says the step under way at once, as an ongoing notification of its own (run-<n>): the
// handler's last word ends it (say).
func (p *Plugin) progress(text string) {
	if p.going == "" {
		p.notices++
		p.going = fmt.Sprintf("run-%d", p.notices)
	}
	p.goingAs = text
	p.notify(plugin.Notice{ID: p.going, Message: "AutoDB: " + text, Ongoing: true})
}

// say sends what the step left to say, after its handler: the status, then the problem, each a
// notification. The first ends the step's ongoing one; a step under way that said nothing more ends
// it as done, so no toast is left running.
func (p *Plugin) say() {
	var said []string
	for _, s := range []string{p.status, p.problem} {
		if s != "" {
			said = append(said, s)
		}
	}
	id := p.going
	if id != "" && len(said) == 0 {
		said = []string{strings.TrimSuffix(p.goingAs, "…") + ": done"}
	}
	p.status, p.problem, p.going, p.goingAs = "", "", "", ""
	for i, s := range said {
		n := plugin.Notice{Message: "AutoDB: " + s}
		if i == 0 {
			n.ID = id
		}
		p.notify(n)
	}
}

// notify sends n, cut to the 1 KiB AutoDoc takes (a long error would be refused whole); before the
// host is there it is dropped, and an AutoDoc that refuses it changes nothing here.
func (p *Plugin) notify(n plugin.Notice) {
	if p.host == nil {
		return
	}
	if len(n.Message) > plugin.MaxNoticeBytes {
		n.Message = cut(n.Message, plugin.MaxNoticeBytes) // table.go's, as a cell is
	}
	_ = p.host.Notify(n)
}
