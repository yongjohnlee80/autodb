// Command autodoc-autodb is AutoDB as an AutoDoc plugin: AutoDoc starts it from its Plugins menu
// (plugin.toml at the repository's top) and the two speak AutoDoc's plugin protocol over stdin and
// stdout. Run by hand, it has no host to speak to.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/yongjohnlee80/autodoc/plugin"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodocplugin"
)

// version is the AutoDB release this plugin was built from, stamped by the build helper
// (-ldflags "-X main.version=<tag>"); "dev" when the commit has no release tag.
var version = "dev"

func main() {
	if err := plugin.Serve(context.Background(), autodocplugin.New(version)); err != nil {
		fmt.Fprintln(os.Stderr, "autodoc-autodb:", err)
		os.Exit(1)
	}
}
