// ConnectionForm.qml — add or edit a connection.
//
// Adding asks name, engine, DSN; editing asks name, proxy exposure and the
// capability profile. The fields a mode does not use are hidden, not greyed.

Dialog {
    title: App.connFormTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    helpText: App.connFormError
    Flex {
        direction: Tui.Vertical
        Text { text: "name" }
        TextField { id: name; text: App.connFormName }
        Text { text: "engine"; visible: App.connFormAdding }
        ComboBox { id: engine; visible: App.connFormAdding; model: App.engines; textRole: "label"; valueRole: "id" }
        Text { text: "DSN"; visible: App.connFormAdding }
        TextField { id: dsn; visible: App.connFormAdding; placeholderText: "postgres://user@host/db" }
        Text { text: "proxy (front door)"; visible: App.connFormEditing }
        ComboBox { id: proxy; visible: App.connFormEditing; model: App.proxyChoices; textRole: "label"; valueRole: "id" }
        Text { text: "profile"; visible: App.connFormEditing }
        ComboBox { id: profile; visible: App.connFormEditing; model: App.profileChoices; textRole: "label"; valueRole: "id" }
    }
    onAccepted: App.saveConnection(name.text, engine.currentValue, dsn.text, proxy.currentValue, profile.currentValue)
}
