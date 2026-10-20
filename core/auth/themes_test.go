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
	for _, ui := range []string{
		"github.com/yongjohnlee80/golib/tui",
		"github.com/yongjohnlee80/golib/tui/decl",
		"github.com/yongjohnlee80/golib/tui/widget",
		"github.com/yongjohnlee80/golib/gui",
	} {
		if slices.Contains(deps, ui) {
			t.Errorf("the frontdoor links %s", ui)
		}
	}
}
