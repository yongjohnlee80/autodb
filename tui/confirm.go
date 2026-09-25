package tui

import "fmt"

// A QUESTION BEFORE AN ACTION — delete this, open the front door on that.
//
// One confirmation card (dialogs/Confirm.qml), reused: its title, its text
// and its two answers are the question's. The answer that acts is never the
// default and never under the cursor by position alone: the card is a
// DialogButtonBox, which has no default, so a stray Enter does nothing.
// The action runs after the card has closed, so a card it opens is not
// stacked above a closing one.

// confirmState is the card's sources, asking nothing.
func confirmState() map[string]any {
	return map[string]any{
		"App.confirmTitle": "",
		"App.confirmText":  "",
		"App.confirmYes":   "&Yes",
		"App.confirmNo":    "&No",
	}
}

// confirm asks, and runs then if the answer is yes.
func (h *Host) confirm(title, text, yes, no string, then func()) {
	h.confirmThen = then
	if err := h.p.SetMany(map[string]any{
		"App.confirmTitle": title, "App.confirmText": text,
		"App.confirmYes": yes, "App.confirmNo": no,
	}); err != nil {
		h.keep(err)
		return
	}
	h.open("confirm")
}

// confirmed is App.confirmed(answer): "yes" or "no".
func (h *Host) confirmed(answer string) error {
	then := h.confirmThen
	h.confirmThen = nil
	switch answer {
	case "yes":
		if then != nil {
			h.p.Post(then)
		}
	case "no":
	default:
		return fmt.Errorf("App.confirmed: %q is not yes or no", answer)
	}
	return nil
}
