// Leader.qml — SPC's which-key menu: every command offered here, by key.
//
// A VIEW of the catalog. App.leader is a host model, one row per command with
// a leader key; App.leaderText is the same rows as the card shows them — key,
// label, and a disabled command's reason. A hidden command is in neither.
//
// The rows are TEXT, not a list: a list takes the keyboard and eats j, k and
// g, which are leader keys. With nothing focusable in it, every key reaches
// the dialog's Shortcuts — one per row — and a key runs its command through
// the catalog (App.leaderKey). A disabled command's key leaves the card open
// and says why; Esc closes it.

Dialog {
    id: leader
    title: "SPC — commands"
    helpText: "a key runs its command · Esc closes"
    Text { text: App.leaderText }
    Repeater {
        model: App.leader
        Shortcut { sequence: model.key; onActivated: App.leaderKey(model.id) }
    }
}
