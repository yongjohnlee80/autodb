// Leader.qml — SPC's which-key menu: every command offered here, by key.
//
// A VIEW of the catalog (App.leader, a host model, one row per command:
// key, label, enabled, reason, id). A disabled row stays, dimmed, saying why,
// and its key does nothing; a hidden one is not in the model. Pressing a key
// runs its command through the catalog; q dismisses only when q is unbound.

Popup {
    id: leader
    modal: true
    Frame {
        title: "SPC — commands"
        Flex {
            direction: Tui.Vertical
            Repeater {
                model: App.leader
                Flex {
                    direction: Tui.Horizontal
                    enabled: model.enabled
                    Text { text: model.key }
                    Text { text: model.label }
                    Shortcut {
                        sequence: model.key
                        enabled: model.enabled
                        onActivated: App.run(model.id)
                    }
                }
            }
        }
    }
}
