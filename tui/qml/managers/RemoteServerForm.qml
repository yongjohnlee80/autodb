// One remote server, as this computer reaches it: kept in remotes.toml,
// which holds no secret. The host key is pinned by the first connect.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 85
    title: App.serverFormTitle
    width: 80
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.serverFormError
    onRejected: App.serverFormClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: "name (how the menus show it)" }
        TextField { id: name; text: App.serverFormName }
        Text { text: "host (name or address)" }
        TextField { id: host; text: App.serverFormHost }
        Text { text: "port (blank for 7422)" }
        TextField { id: port; text: App.serverFormPort }
        Text { text: "your autodb user there" }
        TextField { id: user; text: App.serverFormUser }
        Text { text: "SSH key file (its .pub must be registered on your profile there)" }
        TextField { id: keyFile; text: App.serverFormKeyFile }
        Text { text: "signing" }
        ComboBox { id: auth; model: App.serverAuthChoices; textRole: "label"; valueRole: "id"; currentIndex: App.serverFormAuth }
        Text { text: "host key" }
        Text { text: App.serverFormPin }
    }
    onAccepted: App.saveServer(name.text, host.text, port.text, user.text, keyFile.text, auth.currentValue)
}
