// One form for a new user, role, passphrase reset, or connection grant.
Dialog {
    title: App.userFormTitle
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.userFormError
    onOpened: { name.clear(); passphrase.clear() }
    onRejected: { passphrase.clear(); App.userFormCancelled() }
    Flex {
        direction: Tui.Vertical
        Text { text: "name"; visible: App.userFormNaming }
        TextField { id: name; visible: App.userFormNaming }
        Text { text: "role"; visible: App.userFormRoling }
        ComboBox { id: role; visible: App.userFormRoling; model: App.roles; textRole: "label"; valueRole: "id"; currentIndex: App.userFormRoleIndex }
        Text { text: "connection"; visible: App.userFormGranting }
        ComboBox { id: conn; visible: App.userFormGranting; model: App.userConnections; textRole: "label"; valueRole: "id"; currentIndex: App.userFormConnIndex }
        Text { text: "passphrase (min 8 characters)"; visible: App.userFormPassphrase }
        TextField { id: passphrase; visible: App.userFormPassphrase; echoMode: TextInput.Password }
    }
    onAccepted: { App.saveUser(name.text, role.currentValue, conn.currentValue, passphrase.text); passphrase.clear() }
}
