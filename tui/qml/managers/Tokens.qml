// Access tokens for the signed-in account; historical revocations are hidden.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "my access tokens"
    dim: false
    helpText: App.tokensStatus
    onRejected: App.tokensClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.tokenRows
            TableViewColumn { role: "name"; title: "NAME"; width: 18 }
            TableViewColumn { role: "expires"; title: "EXPIRES"; width: 12 }
            TableViewColumn { role: "lastUsed"; title: "LAST USED"; width: 12 }
            TableViewColumn { role: "ips"; title: "IPS"; width: 16 }
            TableViewColumn { role: "state"; title: "STATE"; width: 0 }
        }
    }
    DialogButtonBox {
        Button { text: "&Create"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.tokenCreate() }
        Button { text: "&Revoke"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.tokenRevoke(table.currentIndex) }
        Button { text: App.showRevokedLabel; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.tokenToggleRevoked() }
        Button { text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
