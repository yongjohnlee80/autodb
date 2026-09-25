// UserForm.qml — a new user, a role change, a passphrase reset, or a grant:
// one form, the fields its purpose needs.

Dialog {
    title: App.userFormTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    helpText: App.userFormError
    Flex {
        direction: Tui.Vertical
        Text { text: "name"; visible: App.userFormNaming }
        TextField { id: name; visible: App.userFormNaming }
        Text { text: "role"; visible: App.userFormRoling }
        ComboBox { id: role; visible: App.userFormRoling; model: App.roles; textRole: "label"; valueRole: "id" }
        Text { text: "connection"; visible: App.userFormGranting }
        ComboBox { id: conn; visible: App.userFormGranting; model: App.connections; textRole: "label"; valueRole: "id" }
        Text { text: "passphrase"; visible: App.userFormPassphrase }
        TextField { id: passphrase; visible: App.userFormPassphrase; echoMode: TextInput.Password }
    }
    onAccepted: App.saveUser(name.text, role.currentValue, conn.currentValue, passphrase.text)
}
