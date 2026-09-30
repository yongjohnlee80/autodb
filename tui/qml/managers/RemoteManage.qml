// Remote › Manage…: its sections chosen at the top, each offered only where it
// applies (App.manageSections). The shown section's table and buttons are the
// only ones visible; the host ignores a hidden section's button.
Dialog {
    closeOnQ: true
    maxWidthPercent: 90
    maxHeightPercent: 85
    title: "manage remote access"
    dim: false
    helpText: App.manageStatus
    onRejected: App.manageClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: "section" }
        ComboBox {
            id: section
            model: App.manageSections
            textRole: "label"
            valueRole: "key"
            currentIndex: App.manageSectionIndex
            onActivated: App.manageSection(index)
        }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: servers
            visible: App.manageOnServers
            model: App.serverRows
            TableViewColumn { role: "name"; title: "SERVER"; width: 24 }
            TableViewColumn { role: "address"; title: "ADDRESS"; width: 22 }
            TableViewColumn { role: "user"; title: "USER"; width: 12 }
            TableViewColumn { role: "sshKey"; title: "SSH KEY"; width: 20 }
            TableViewColumn { role: "pin"; title: "HOST KEY"; width: 22 }
            TableViewColumn { role: "device"; title: "DEVICE"; width: 0 }
        }
        Text { visible: App.manageOnMine; text: App.manageMineTitle }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: mine
            visible: App.manageOnMine
            model: App.mineRows
            TableViewColumn { role: "label"; title: "LABEL"; width: 16 }
            TableViewColumn { role: "fingerprint"; title: "KEY"; width: 22 }
            TableViewColumn { role: "type"; title: "TYPE"; width: 12 }
            TableViewColumn { role: "device"; title: "DEVICE"; width: 24 }
            TableViewColumn { role: "lastIP"; title: "LAST IP"; width: 16 }
            TableViewColumn { role: "lastSeen"; title: "LAST SEEN"; width: 0 }
        }
    }
    DialogButtonBox {
        Button { text: "&Add"; visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverAdd() }
        Button { text: "&Edit"; visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverEdit(servers.currentIndex) }
        Button { text: "Re&move"; visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverRemove(servers.currentIndex) }
        Button { text: "Forget &host key"; visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverForget(servers.currentIndex) }
        Button { text: "Add &key"; visible: App.manageOnMine; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyAdd() }
        Button { text: "&Label"; visible: App.manageOnMine; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyLabel(mine.currentIndex) }
        Button { text: "Re&voke key"; visible: App.manageOnMine; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyRevoke(mine.currentIndex) }
        Button { text: "Revoke &device"; visible: App.manageOnMine; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.deviceRevoke(mine.currentIndex) }
        Button { text: "Refre&sh"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.manageRefresh() }
        Button { text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
