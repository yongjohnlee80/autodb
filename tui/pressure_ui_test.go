package tui_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/pressure"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	"github.com/yongjohnlee80/golib/tui/decl/decltest"
)

type pressureFunc func(context.Context) (pressure.Snapshot, error)

func (f pressureFunc) Pressure(ctx context.Context) (pressure.Snapshot, error) { return f(ctx) }

func pressureScreen(t *testing.T, source pressureFunc) (*tuiapp.Host, *decltest.Screen) {
	t.Helper()
	h, s := tuiapp.RunHost(t, tuiapp.NewSession(seeded(t), logger.Nop{}, nil),
		tuiapp.PersonalNotesIn(t.TempDir()), tuiapp.Options{PressureSource: source}, 120, 32)
	loginAs(t, s, "root", rootPass)
	s.WaitFor(t, "signed in", func(string) bool { return h.Auth() == "signed-in" })
	return h, s
}

func TestPressureRaisedAndNormalRowsUseDifferentThemeColors(t *testing.T) {
	_, s := pressureScreen(t, func(context.Context) (pressure.Snapshot, error) {
		return pressure.Snapshot{Sessions: pressure.Row{Value: 9, Cap: 10, Raised: true},
			Conns: pressure.Row{Value: 2, Cap: 10}}, nil
	})
	s.Keys(t, key(' '), key('P'))
	s.WaitForText(t, "9 / 10")
	grid := s.Backend.Snapshot()
	var raised, normal *int
	var fgRaised, fgNormal any
	for y, row := range strings.Split(s.String(), "\n") {
		for _, item := range []struct {
			name   string
			target **int
			color  *any
		}{
			{"sessions", &raised, &fgRaised}, {"connections", &normal, &fgNormal},
		} {
			if x := strings.Index(row, item.name); x >= 0 {
				v := y
				*item.target = &v
				*item.color = grid[y][x].Attrs.FG
			}
		}
	}
	if raised == nil || normal == nil || fgRaised == fgNormal {
		t.Fatal("raised pressure row was not distinguished by the imported theme color")
	}
}

func TestPressureCloseAndReopenCannotApplyAnOldPoll(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	_, s := pressureScreen(t, func(context.Context) (pressure.Snapshot, error) {
		n := calls.Add(1)
		if n == 2 {
			started <- struct{}{}
			<-release
		} // ignore cancellation deliberately
		return pressure.Snapshot{Sessions: pressure.Row{Value: int(n), Cap: 10}}, nil
	})
	s.Keys(t, key(' '), key('P'))
	s.WaitForText(t, "1 / 10")
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("open view never polled")
	}
	s.Keys(t, esc())
	s.WaitFor(t, "pressure closed", func(sc string) bool { return !strings.Contains(sc, "┌ pressure ") })
	s.Keys(t, key(' '), key('P'))
	s.WaitForText(t, "3 / 10")
	close(release)
	time.Sleep(100 * time.Millisecond)
	if sc := s.String(); !strings.Contains(sc, "3 / 10") || strings.Contains(sc, "2 / 10") {
		t.Fatal("a canceled older pressure poll replaced the reopened view")
	}
}
