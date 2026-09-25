// Tokens.qml — your access tokens: create, revoke; revoked ones hidden until
// asked for.

Dialog {
    title: App.tokensTitle
    standardButtons: Dialog.Close
    helpText: App.tokensStatus
    Flex {
        direction: Tui.Vertical
        TableView {
            id: table
            model: App.tokenRows
            TableViewColumn { role: "name";     title: "NAME";      width: 18 }
            TableViewColumn { role: "expires";  title: "EXPIRES";   width: 12 }
            TableViewColumn { role: "lastUsed"; title: "LAST USED"; width: 12 }
            TableViewColumn { role: "ips";      title: "IPS";       width: 16 }
            TableViewColumn { role: "state";    title: "STATE";     width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&Create"; onClicked: App.tokenCreate() }
            Button { label: "Re&voke"; onClicked: App.tokenRevoke(table.currentIndex) }
            Button { label: App.showRevokedLabel; onClicked: App.tokenToggleRevoked() }
        }
    }
}
