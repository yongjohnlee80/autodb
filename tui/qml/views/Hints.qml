// Hints.qml — `?`: the keys that work where you are.
//
// The host fills App.hints for the place the keyboard is, then opens this
// card. q or Esc closes it.

Dialog {
    id: hints
    closeOnQ: true
    dim: false
    title: "keys here"
    Text { wrapMode: Tui.WordWrap; text: App.hints }
}
