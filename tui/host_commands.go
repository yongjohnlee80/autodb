package tui

import (
	"fmt"

	"github.com/yongjohnlee80/golib/decl"
	"github.com/yongjohnlee80/golib/parse/qml"
)

// THE APP SINGLETON'S COMMANDS — what the document invokes.
//
// ONE handler, App.run(id), and it goes through the catalog's single
// activation path: the command is re-resolved — offered, for this user, in
// this state — immediately before it runs. There is no second handler map to
// drift from the catalog, and no way for a document to run what the catalog
// would not offer.
func (h *Host) commands() map[string]decl.HandlerFunc {
	return map[string]decl.HandlerFunc{
		"App.run": func(args []qml.SpecValue) error {
			if len(args) != 1 || args[0].Kind != qml.SpecValueString {
				return fmt.Errorf("App.run takes a command id")
			}
			id := CommandID(args[0].Raw)
			if _, known := h.catalog.Command(id); !known {
				return fmt.Errorf("App.run: %q is not a command", id)
			}
			if !h.catalog.runIfOffered(h, id) {
				h.setStatus("not available now: " + string(id))
			}
			return nil
		},
	}
}
