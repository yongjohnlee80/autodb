package tui

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
)

// SWITCHING THEME — Options › Theme.
//
// A theme is chosen by one line of main.qml, its import:
//
//	import autodb.theme.retro 1.0
//
// so switching theme at runtime is that line, rewritten, and the layout
// reloaded — the path a hot reload takes. Nothing the theme does not touch is
// rebuilt: the query, the cursor and the results stay as they were.
//
// Under -dev the file on disk stays the authority: the switch rewrites the
// layout in memory, and the next save of main.qml brings back its own import.

// themeImport is a theme import line; its group is the theme's name.
var themeImport = regexp.MustCompile(`(?m)^import autodb\.theme\.([a-z][a-z0-9]*) ` +
	regexp.QuoteMeta(moduleVersion))

// themeOf is the theme a layout imports, "" for none.
func themeOf(src []byte) string {
	if m := themeImport.FindSubmatch(src); m != nil {
		return string(m[1])
	}
	return ""
}

// pickTheme records the theme a layout imports as the one the screen wears.
func (h *Host) pickTheme(src []byte) string {
	h.theme = themeOf(src)
	return h.theme
}

// themeState is App.theme: the theme the layout imports.
func themeState(theme string) map[string]any {
	return map[string]any{"App.theme": theme}
}

// useTheme switches to the named theme; one the program does not ship is
// refused, and the screen stays as it was.
func (h *Host) useTheme(name string) {
	themes := qmlFiles
	if h.dev != "" {
		themes = os.DirFS(h.dev)
	}
	if _, err := fs.Stat(themes, path.Join("themes", name+".qml")); err != nil {
		h.setStatus(fmt.Sprintf("no theme %q", name))
		return
	}
	src := h.layoutSrc
	if h.dev != "" {
		b, err := os.ReadFile(path.Join(h.dev, "main.qml"))
		if err != nil {
			h.setStatus("theme: " + err.Error())
			return
		}
		src = b
	}
	if themeOf(src) == "" {
		h.setStatus("main.qml imports no theme to switch")
		return
	}
	next := themeImport.ReplaceAll(src, []byte("import autodb.theme."+name+" "+moduleVersion))
	// AFTER the handler: a menu row's signal is still being emitted, and the
	// engine reconciles only between emissions.
	h.p.Post(func() {
		if _, err := h.p.Reload(next); err != nil {
			h.setStatus("theme: " + err.Error())
			return
		}
		if h.dev == "" {
			h.layoutSrc = next
		}
		h.theme = name
		for k, v := range themeState(name) {
			h.set(k, v)
		}
		h.reproject() // the Theme menu's mark follows it
		h.setStatus("theme: " + name)
	})
}
