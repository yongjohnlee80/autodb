package auth

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/decl/themes/qmlfiles"
)

// The account accepts golib's themes, every one: a list of its own would refuse a theme golib
// adds, which the TUI offers.
func TestTheThemesAreGolibs(t *testing.T) {
	if !slices.Equal(Themes, qmlfiles.Names()) {
		t.Fatalf("auth.Themes %v, golib ships %v", Themes, qmlfiles.Names())
	}
}

// The frontdoor links this package and none of golib's UI: the theme names come from a package
// that imports none of it.
func TestTheFrontdoorLinksNoUI(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "../../frontdoor/...")
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/yongjohnlee80/autodb/core/auth") {
		t.Fatal("the frontdoor no longer links core/auth; this cell checks nothing")
	}
	// all of golib's UI, by prefix; the two leaf packages core/auth reads are the named exemptions,
	// the theme names and the panel geometry check, which import nothing of golib
	leaves := []string{
		"github.com/yongjohnlee80/golib/tui/decl/themes/qmlfiles",
		"github.com/yongjohnlee80/golib/tui/panelgeometry",
	}
	for _, dep := range deps {
		ui := dep == "github.com/yongjohnlee80/golib/tui" || strings.HasPrefix(dep, "github.com/yongjohnlee80/golib/tui/") ||
			dep == "github.com/yongjohnlee80/golib/gui" || strings.HasPrefix(dep, "github.com/yongjohnlee80/golib/gui/")
		if ui && !slices.Contains(leaves, dep) {
			t.Errorf("the frontdoor links %s", dep)
		}
	}
}
