package remote_test

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/remote"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func acceptFrom(t *testing.T, f *remote.FanIn) net.Conn {
	t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		if c, err := f.Accept(); err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("nothing accepted")
		return nil
	}
}

// Connections from the base and from an added source both arrive; a removed
// source stops arriving and the base still serves.
func TestAFanInServesItsBaseAndItsSources(t *testing.T) {
	base, extra := listen(t), listen(t)
	f := remote.NewFanIn(base)
	defer f.Close()
	f.Add(extra)
	for _, ln := range []net.Listener{base, extra} {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		acceptFrom(t, f)
	}
	f.Remove(extra)
	if c, err := net.Dial("tcp", extra.Addr().String()); err == nil {
		c.Close()
		t.Fatal("a removed source still accepts")
	}
	c, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	acceptFrom(t, f)
	if f.Addr().String() != base.Addr().String() {
		t.Fatalf("Addr %v, want the base's %v", f.Addr(), base.Addr())
	}
}

// The base failing ends the fan-in; Close closes every source.
func TestTheBaseFailingEndsTheFanIn(t *testing.T) {
	base, extra := listen(t), listen(t)
	f := remote.NewFanIn(base)
	f.Add(extra)
	_ = base.Close()
	done := make(chan error, 1)
	go func() { _, err := f.Accept(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after the base failed: %v; want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fan-in outlived its base")
	}
	if c, err := net.Dial("tcp", extra.Addr().String()); err == nil {
		c.Close()
		t.Fatal("a source outlived the fan-in")
	}
}
