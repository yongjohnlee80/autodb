// The token is bound to one offered connection. The backend revalidates it.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.tokenForm.title")
    width: 72
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.tokenFormError
    onRejected: App.tokenFormClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.tokenForm.text") }
        TextField { id: name; text: App.tokenFormName }
        Text { text: qsTrId("autodb.tokenForm.text2") }
        TextField { id: days; text: App.tokenFormDays }
        Text { text: qsTrId("autodb.tokenForm.text3") }
        TextField { id: ips; text: App.tokenFormIPs }
        Text { text: qsTrId("autodb.tokenForm.text4") }
        ComboBox { id: conn; model: App.tokenConnections; textRole: "label"; valueRole: "id"; currentIndex: App.tokenConnectionIndex }
        Text { text: qsTrId("autodb.tokenForm.text5"); visible: App.tokenFormCleartext }
        TextField { id: cleartext; text: App.tokenFormDebug; visible: App.tokenFormCleartext; placeholderText: qsTrId("autodb.tokenForm.placeholderText") }
    }
    onAccepted: App.mintToken(name.text, days.text, ips.text, conn.currentValue, cleartext.text)
}
