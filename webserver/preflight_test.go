package webserver

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// The two probe classes need different remedies, so they must not collapse into
// one message. A single "cannot reach the daemon" would send an operator to start
// a server on an address that is already occupied (ADR-0061 §2.2).
func TestPreflight_DistinguishesNoDaemonFromForeignOccupant(t *testing.T) {
	t.Parallel()

	t.Run("nothing listening", func(t *testing.T) {
		t.Parallel()
		// A port that was just closed: reliably refuses without depending on a
		// port nobody happens to be using.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		_, err = Preflight(context.Background(), "tcp", addr)
		if !errors.Is(err, ErrNoDaemon) {
			t.Fatalf("err = %v, want ErrNoDaemon", err)
		}
		if errors.Is(err, ErrForeignOccupant) {
			t.Error("a dial failure was classed as a foreign occupant")
		}
		// The remedy has to be in the message, because the message is the whole
		// interface here.
		if !strings.Contains(err.Error(), "autodb --serve") {
			t.Errorf("the message does not name the fix: %v", err)
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("the message does not name the address: %v", err)
		}
	})

	t.Run("something else answers", func(t *testing.T) {
		t.Parallel()
		// A listener that accepts and says nothing: the shape of a stale socket or
		// a foreign process.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, aerr := ln.Accept()
				if aerr != nil {
					return
				}
				_ = c.Close() // accept, then hang up
			}
		}()

		_, err = Preflight(context.Background(), "tcp", ln.Addr().String())
		if !errors.Is(err, ErrForeignOccupant) {
			t.Fatalf("err = %v, want ErrForeignOccupant", err)
		}
		if errors.Is(err, ErrNoDaemon) {
			t.Error("an occupied address was classed as no daemon")
		}
		// The critical negative: this must NOT tell the operator to start a
		// daemon, because the address cannot be bound.
		if strings.Contains(err.Error(), "autodb --serve") {
			t.Errorf("the message sends the operator to start a daemon on an "+
				"address that is already taken: %v", err)
		}
	})
}
