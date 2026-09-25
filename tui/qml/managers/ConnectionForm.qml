// ConnectionForm.qml — add or edit a connection.
//
// Adding asks name, engine, DSN; editing asks name, proxy exposure and the
// capability profile, each starting on what the connection has now. The
// fields a mode does not use are hidden, not greyed. Editing sends only what
// changed; opening the front door asks once more, after this closes.

Dialog {
    title: App.connFormTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.connFormError
    Flex {
        direction: Tui.Vertical
        Text { text: "name" }
        TextField { id: name; text: App.connFormName }
        Text { text: "engine"; visible: App.connFormAdding }
        ComboBox { id: engine; visible: App.connFormAdding; model: App.engines; textRole: "label"; valueRole: "id"; currentIndex: App.connFormEngine; placeholderText: "choose an engine" }
        Text { text: "DSN (stored encrypted at rest)"; visible: App.connFormAdding }
        TextField { id: dsn; visible: App.connFormAdding; echoMode: TextInput.Password; placeholderText: "postgres://user@host/db" }
        Text { text: "proxy — front door reachability"; visible: App.connFormEditing }
        ComboBox { id: proxy; visible: App.connFormEditing; model: App.proxyChoices; textRole: "label"; valueRole: "id"; currentIndex: App.connFormProxy }
        Text { text: "capability profile — what SQL clients may send"; visible: App.connFormEditing }
        ComboBox { id: profile; visible: App.connFormEditing; model: App.profileChoices; textRole: "label"; valueRole: "id"; currentIndex: App.connFormProfile }
    }
    onOpened: dsn.clear()
    onAccepted: App.saveConnection(name.text, engine.currentValue, dsn.text, proxy.currentValue, profile.currentValue)
}
