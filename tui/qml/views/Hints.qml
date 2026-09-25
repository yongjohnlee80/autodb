// Hints.qml — `?`: the keys that work where you are.
//
// Bottom-right, any key closes it. The host fills App.hints for the topmost
// dialog, or else for the pane in use.

Popup {
    id: hints
    modal: true
    Frame {
        title: "keys here"
        Text { text: App.hints }
    }
}
