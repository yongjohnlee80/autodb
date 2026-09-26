// Admin-only account list: actions use the pinned manager identity.
Dialog {
    title: "users"
    dim: false
    helpText: App.usersStatus
    onRejected: App.usersClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
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
        Button { text: "&Add"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userAdd() }
        Button { text: "&Role"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userRole(table.currentIndex) }
        Button { text: "&Passphrase"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userResetPassphrase(table.currentIndex) }
        Button { text: "&Toggle"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userToggle(table.currentIndex) }
        Button { text: "Grant &connection"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userGrant(table.currentIndex) }
        Button { text: "&IPs"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userIPs(table.currentIndex) }
        Button { text: "Remo&ve"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.userRemove(table.currentIndex) }
        Button { text: "&Done"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
