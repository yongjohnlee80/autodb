// Hints.qml — `?`: the keys that work where you are.
//
// The host fills App.hints for the place the keyboard is, then opens this
// card. Esc closes it.

Popup {
    id: hints
    modal: true
    dim: false
    Frame {
        title: "keys here"
        Text { wrapMode: Tui.WordWrap; text: App.hints }
    }
}
