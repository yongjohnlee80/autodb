// The history listing's filter: the server narrows the listing, so every
// field here becomes part of the query. The choices are what the caller can
// see; "any" leaves a field out.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "filter history"
    width: 60
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    onRejected: App.historyFilterCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: "connection" }
        ComboBox { id: conn; model: App.historyConnChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterConnIndex }
        Text { text: "workspace" }
        ComboBox { id: space; model: App.historySpaceChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterSpaceIndex }
        Text { text: "user"; visible: App.historyFilterUsers }
        ComboBox { id: user; visible: App.historyFilterUsers; model: App.historyUserChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterUserIndex }
        Text { text: "status" }
        ComboBox { id: status; model: App.historyStatusChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterStatusIndex }
        Text { text: "from (YYYY-MM-DD)" }
        TextField { id: from }
        Text { text: "to (YYYY-MM-DD, inclusive)" }
        TextField { id: to }
    }
    onAccepted: App.historyFilterApply(conn.currentValue, space.currentValue, user.currentValue, status.currentValue, from.text, to.text)
}
