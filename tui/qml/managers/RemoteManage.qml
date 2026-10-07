// Remote › Manage…: its sections chosen at the top, each offered only where it
// applies (App.manageSections): Servers, the SSH key tables (My devices,
// Devices, a user's SSH keys), Remote activity, Remote Control and Blocked
// IPs. The shown section's content and buttons are the only ones visible; the
// host ignores a hidden section's button.
Dialog {
    closeOnQ: true
    maxWidthPercent: 90
    maxHeightPercent: 85
    title: qsTrId("autodb.remoteManage.title")
    dim: false
    helpText: App.manageStatus
    onRejected: App.manageClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.remoteManage.text") }
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
        Text { visible: App.manageOnKeys; text: App.manageKeysTitle }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: keys
            visible: App.manageOnKeys
            model: App.keyRows
            TableViewColumn { role: "user"; title: "USER"; width: 12 }
            TableViewColumn { role: "label"; title: "LABEL"; width: 16 }
            TableViewColumn { role: "fingerprint"; title: "KEY"; width: 22 }
            TableViewColumn { role: "type"; title: "TYPE"; width: 12 }
            TableViewColumn { role: "device"; title: "DEVICE"; width: 24 }
            TableViewColumn { role: "lastIP"; title: "LAST IP"; width: 16 }
            TableViewColumn { role: "lastSeen"; title: "LAST SEEN"; width: 0 }
        }
        Text { visible: App.manageOnControl; wrapMode: Tui.WordWrap; text: App.controlText }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: blocks
            visible: App.manageOnBlocks
            model: App.blockRows
            TableViewColumn { role: "prefix"; title: "ADDRESS"; width: 24 }
            TableViewColumn { role: "failures"; title: "REFUSED"; width: 8 }
            TableViewColumn { role: "state"; title: "STATE"; width: 24 }
            TableViewColumn { role: "last"; title: "LAST REFUSAL"; width: 18 }
            TableViewColumn { role: "reason"; title: "REASON"; width: 0 }
        }
        Text { visible: App.manageOnActivity; text: qsTrId("autodb.remoteManage.text2") }
        ComboBox {
            id: kind
            visible: App.manageOnActivity
            model: App.activityKinds
            textRole: "label"
            valueRole: "key"
            currentIndex: App.activityKindIndex
            onActivated: App.activityKind(index)
        }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: activity
            visible: App.manageOnActivity
            model: App.activityRows
            TableViewColumn { role: "when"; title: "WHEN"; width: 17 }
            TableViewColumn { role: "who"; title: "WHO"; width: 12 }
            TableViewColumn { role: "action"; title: "WHAT"; width: 20 }
            TableViewColumn { role: "ip"; title: "IP"; width: 16 }
            TableViewColumn { role: "detail"; title: "DETAIL"; width: 0 }
        }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.remoteManage.text3"); visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverAdd() }
        Button { text: qsTrId("autodb.remoteManage.text4"); visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverEdit(servers.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text5"); visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverRemove(servers.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text6"); visible: App.manageOnServers; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.serverForget(servers.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text7"); visible: App.manageKeysEdit; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyAdd() }
        Button { text: qsTrId("autodb.remoteManage.text8"); visible: App.manageKeysEdit; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyLabel(keys.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text9"); visible: App.manageOnKeys; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyRevoke(keys.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text10"); visible: App.manageOnKeys; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.deviceRevoke(keys.currentIndex) }
        Button { text: App.controlToggleLabel; visible: App.manageOnControl; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.controlToggle() }
        Button { text: qsTrId("autodb.remoteManage.text11"); visible: App.manageOnBlocks; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.blockUnblock(blocks.currentIndex) }
        Button { text: qsTrId("autodb.remoteManage.text12"); visible: App.manageOnActivity; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.activityNext() }
        Button { text: qsTrId("autodb.remoteManage.text13"); visible: App.manageOnActivity; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.activityPrev() }
        Button { text: qsTrId("autodb.remoteManage.text14"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.manageRefresh() }
        Button { text: qsTrId("autodb.remoteManage.text15"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
