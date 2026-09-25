// Hints.qml — `?`: the keys that work where you are.
//
// The host fills App.hints for the place the keyboard is, then opens this
// card. Esc closes it.

Popup {
    id: hints
    modal: true
    Frame {
        title: "keys here"
        Text { text: App.hints }
    }
}
