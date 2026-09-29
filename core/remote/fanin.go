package remote

import (
	"net"
	"sync"
)

// FanIn is the one listener the RPC server serves on: the local listener,
// plus the remote listener while it runs. One server behind both keeps one
// instance, one shutdown and one session registry for every connection; which
// surface a connection is on is its own fact (Conn), not the listener's.
//
// The base listener's failure ends the fan-in: that is the server's own
// surface, and a server that can no longer accept locally must stop. A source
// added later failing ends only that source.
type FanIn struct {
	base  net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	closed  bool
	sources map[net.Listener]struct{}
}

// NewFanIn serves base, and whatever is added later.
func NewFanIn(base net.Listener) *FanIn {
	f := &FanIn{base: base, conns: make(chan net.Conn), done: make(chan struct{}),
		sources: map[net.Listener]struct{}{}}
	go f.pump(base, true)
	return f
}

// Add serves src until it fails, is removed, or the fan-in closes.
func (f *FanIn) Add(src net.Listener) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = src.Close()
		return
	}
	f.sources[src] = struct{}{}
	f.mu.Unlock()
	go f.pump(src, false)
}

// Remove closes src and stops serving it. Connections it already handed over
// are not touched.
func (f *FanIn) Remove(src net.Listener) {
	f.mu.Lock()
	delete(f.sources, src)
	f.mu.Unlock()
	_ = src.Close()
}

func (f *FanIn) pump(src net.Listener, base bool) {
	for {
		c, err := src.Accept()
		if err != nil {
			if base {
				_ = f.Close()
			} else {
				f.mu.Lock()
				delete(f.sources, src)
				f.mu.Unlock()
			}
			return
		}
		select {
		case f.conns <- c:
		case <-f.done:
			_ = c.Close()
			return
		}
	}
}

// Accept returns the next connection from any source.
func (f *FanIn) Accept() (net.Conn, error) {
	select {
	case c := <-f.conns:
		return c, nil
	case <-f.done:
		return nil, net.ErrClosed
	}
}

// Close closes the base listener and every source.
//
// In that order, and only then does Accept answer closed: a caller that sees
// the fan-in closed can rely on no source still accepting. (Closing done
// first let Accept report closed while a source was still listening.)
func (f *FanIn) Close() error {
	var err error
	f.once.Do(func() {
		f.mu.Lock()
		f.closed = true
		srcs := f.sources
		f.sources = map[net.Listener]struct{}{}
		f.mu.Unlock()
		err = f.base.Close()
		for src := range srcs {
			_ = src.Close()
		}
		close(f.done)
	})
	return err
}

// Addr is the base listener's address: the one local clients dial.
func (f *FanIn) Addr() net.Addr { return f.base.Addr() }
