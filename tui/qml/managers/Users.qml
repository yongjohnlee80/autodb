// Admin-only account list: actions use the pinned manager identity.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.users.title")
    dim: false
    helpText: App.usersStatus
    onRejected: App.usersClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.userRows
            currentIndex: App.userIndex
            TableViewColumn { role: "id"; title: "ID"; width: 5 }
            TableViewColumn { role: "name"; title: "NAME"; width: 20 }
            TableViewColumn { role: "role"; title: "ROLE"; width: 10 }
            TableViewColumn { role: "state"; title: "STATE"; width: 0 }
        }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.users.text"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userAdd() }
        Button { text: qsTrId("autodb.users.text2"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userRole(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text3"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userResetPassphrase(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text4"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userToggle(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text5"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userGrant(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text6"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userIPs(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text7"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userSSHKeys(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text8"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userRemove(table.currentIndex) }
        Button { text: qsTrId("autodb.users.text9"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
