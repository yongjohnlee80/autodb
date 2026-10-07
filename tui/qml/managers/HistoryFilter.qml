// The history listing's filter: the server narrows the listing, so every
// field here becomes part of the query. The choices are what the caller can
// see; "any" leaves a field out.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.historyFilter.title")
    width: 60
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    onRejected: App.historyFilterCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.historyFilter.text") }
        ComboBox { id: conn; model: App.historyConnChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterConnIndex }
        Text { text: qsTrId("autodb.historyFilter.text2") }
        ComboBox { id: space; model: App.historySpaceChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterSpaceIndex }
        Text { text: qsTrId("autodb.historyFilter.text3"); visible: App.historyFilterUsers }
        ComboBox { id: user; visible: App.historyFilterUsers; model: App.historyUserChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterUserIndex }
        Text { text: qsTrId("autodb.historyFilter.text4") }
        ComboBox { id: status; model: App.historyStatusChoices; textRole: "label"; valueRole: "id"; currentIndex: App.historyFilterStatusIndex }
        Text { text: qsTrId("autodb.historyFilter.text5") }
        TextField { id: from }
        Text { text: qsTrId("autodb.historyFilter.text6") }
        TextField { id: to }
    }
    onAccepted: App.historyFilterApply(conn.currentValue, space.currentValue, user.currentValue, status.currentValue, from.text, to.text)
}
