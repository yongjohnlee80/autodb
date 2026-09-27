// The audit log's filter: every field becomes part of the server's query;
// "any" and an empty field leave it out.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "filter audit log"
    width: 60
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    onRejected: App.auditFilterCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: "connection" }
        ComboBox { id: conn; model: App.auditConnChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterConnIndex }
        Text { text: "workspace" }
        ComboBox { id: space; model: App.auditSpaceChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterSpaceIndex }
        Text { text: "user" }
        ComboBox { id: user; model: App.auditUserChoices; textRole: "label"; valueRole: "id"; currentIndex: App.auditFilterUserIndex }
        Text { text: "actions (comma-separated, e.g. login_failed, connection_archived)" }
        TextField { id: actions }
        Text { text: "from (YYYY-MM-DD)" }
        TextField { id: from }
        Text { text: "to (YYYY-MM-DD, inclusive)" }
        TextField { id: to }
    }
    onAccepted: App.auditFilterApply(conn.currentValue, space.currentValue, user.currentValue, actions.text, from.text, to.text)
}
