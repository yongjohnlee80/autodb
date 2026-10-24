package panel

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodoc/plugin"
)

// cardHandler serves the panel's card as the plugin does: every call is the card's.
type cardHandler struct{ p *Panel }

func (h cardHandler) Open(peer *plugin.Peer, o plugin.Open) { h.p.Card().Open(peer, o) }
func (h cardHandler) Key(k plugin.Key)                      { h.p.Card().Key(k) }
func (h cardHandler) Resize(w, hh int)                      { h.p.Card().Resize(w, hh) }
func (h cardHandler) Theme(t plugin.Theme)                  { h.p.Card().Theme(t) }
func (h cardHandler) Close()                                { h.p.Card().Close() }

// served runs the panel under plugin.ServeConn over two pipes, as AutoDoc runs the plugin, opens it
// at 80x24, and returns the host's link and each frame's text as it arrives.
func served(t *testing.T, p *Panel) (*plugin.Link, <-chan string) {
	t.Helper()
	toPlugin, fromHost, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fromPlugin, toHost, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = plugin.ServeConn(ctx, plugin.FileConn(toPlugin, toHost), cardHandler{p}) }()
	screen := make(chan string, 256)
	link, err := plugin.NewLink(ctx, plugin.FileConn(fromPlugin, fromHost), func(m string, params []any) {
		if m != plugin.MethodFrame {
			return
		}
		if rows, _, err := plugin.ReadFrame(params); err == nil {
			var b strings.Builder
			for _, r := range rows {
				for _, run := range r {
					b.WriteString(run.Text)
				}
				b.WriteByte('\n')
			}
			select {
			case screen <- b.String():
			default: // a test reading no more frames never stalls the link
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })
	theme := plugin.Theme{Name: "a", Colors: map[string]string{"app.window": "#112233", "app.windowText": "#ddeeff", "app.base": "#000000", "app.text": "#ffffff"}}
	if err := link.Notify(ctx, plugin.MethodOpen, plugin.OpenParams(plugin.Open{Protocol: plugin.Protocol, Width: 80, Height: 24, Theme: theme})); err != nil {
		t.Fatal(err)
	}
	return link, screen
}

// until waits for a frame whose text satisfies ok, and reports the last frame when none does.
func until(t *testing.T, screen <-chan string, what string, ok func(string) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	last := ""
	for {
		select {
		case s := <-screen:
			if last = s; ok(s) {
				return
			}
		case <-deadline:
			t.Fatalf("no frame %s; the last:\n%s", what, last)
		}
	}
}

// The confirm dialog answers Enter with No, its default: a question asked before something is
// dropped, or a remote's host key is trusted, is never accepted by Enter alone. The dialog's body
// takes no focus, so its default button takes the start.
func TestTheConfirmDialogAnswersEnterWithNo(t *testing.T) {
	var confirmed atomic.Int32
	p := New(Events{Confirmed: func() { confirmed.Add(1) }})
	link, screen := served(t, p)
	until(t, screen, "showing the panel", func(s string) bool { return strings.Contains(s, "AutoDB") })
	if !p.Do(func(v View) {
		v.Set(map[string]any{"confirmTitle": "Trust the host", "confirmText": "Trust this host key? (the question)"})
		v.Open(DialogConfirm)
	}) {
		t.Fatal("the card is not running")
	}
	until(t, screen, "asking the question", func(s string) bool { return strings.Contains(s, "(the question)") })
	if err := link.Notify(context.Background(), plugin.MethodKey, plugin.KeyParams(plugin.Key{Key: "Enter"})); err != nil {
		t.Fatal(err)
	}
	// answered: the dialog closes, and Confirmed, called on the loop as it closes, has run by then
	until(t, screen, "with the dialog closed", func(s string) bool { return !strings.Contains(s, "(the question)") })
	if n := confirmed.Load(); n != 0 {
		t.Fatalf("Enter confirmed the dialog (%d): want No, its default", n)
	}
}
