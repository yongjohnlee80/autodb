// The audit log's filter: every field becomes part of the server's query;
// "any" and an empty field leave it out.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.auditFilter.title")
    width: 60
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    onRejected: App.auditFilterCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.auditFilter.text") }
        ComboBox { id: conn; model: App.auditConnChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterConnIndex }
        Text { text: qsTrId("autodb.auditFilter.text2") }
        ComboBox { id: space; model: App.auditSpaceChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterSpaceIndex }
        Text { text: qsTrId("autodb.auditFilter.text3") }
        ComboBox { id: user; model: App.auditUserChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterUserIndex }
        Text { text: qsTrId("autodb.auditFilter.text4") }
        TextField { id: actions }
        Text { text: qsTrId("autodb.auditFilter.text5") }
        TextField { id: from }
        Text { text: qsTrId("autodb.auditFilter.text6") }
        TextField { id: to }
    }
    onAccepted: App.auditFilterApply(conn.currentValue, space.currentValue, user.currentValue, actions.text, from.text, to.text)
}
