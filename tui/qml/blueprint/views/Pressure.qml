// Pressure.qml — the server's load, refreshed while open.
//
// NEW: a table (measure | value) instead of hand-aligned text; a raised value
// is its row's `state`, and the palette colours it — the view names no red.
// Stale figures stay, with their age, when a refresh fails.

Dialog {
    title: "pressure"
    standardButtons: Dialog.Close
    helpText: App.pressureAge
    onOpened: App.pressureOpened()
    onClosed: App.pressureClosed()
    TableView {
        model: App.pressure
        TableViewColumn { role: "measure"; title: "MEASURE"; width: 28 }
        TableViewColumn { role: "value"; title: "VALUE"; width: 0 }
    }
}
