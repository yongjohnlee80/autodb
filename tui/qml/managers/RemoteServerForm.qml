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
        Text { text: qsTrId("autodb.remoteServerForm.text") }
        TextField { id: name; text: App.serverFormName }
        Text { text: qsTrId("autodb.remoteServerForm.text2") }
        TextField { id: host; text: App.serverFormHost }
        Text { text: qsTrId("autodb.remoteServerForm.text3") }
        TextField { id: port; text: App.serverFormPort }
        Text { text: qsTrId("autodb.remoteServerForm.text4") }
        TextField { id: user; text: App.serverFormUser }
        Text { text: qsTrId("autodb.remoteServerForm.text5") }
        TextField { id: keyFile; text: App.serverFormKeyFile }
        Text { text: qsTrId("autodb.remoteServerForm.text6") }
        ComboBox { id: auth; model: App.serverAuthChoices; textRole: "label"; valueRole: "id"; currentIndex: App.serverFormAuth }
        Text { text: qsTrId("autodb.remoteServerForm.text7") }
        Text { text: App.serverFormPin }
    }
    onAccepted: App.saveServer(name.text, host.text, port.text, user.text, keyFile.text, auth.currentValue)
}
