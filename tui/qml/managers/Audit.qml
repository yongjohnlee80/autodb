// The audit log, for an admin. The host owns the search, its filter and pages.
Dialog {
    keyScope: "autodb.audit" // its keys' commands (tui/keys.go)
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.audit.title")
    dim: false
    standardButtons: Dialog.Close
    helpText: App.auditStatus
    onRejected: App.auditClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: App.auditSince; visible: App.auditSinceShown }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.auditRows
            TableViewColumn { role: "when"; title: "WHEN"; width: 17 }
            TableViewColumn { role: "who"; title: "WHO"; width: 10 }
            TableViewColumn { role: "action"; title: "ACTION"; width: 22 }
            TableViewColumn { role: "conn"; title: "CONNECTION"; width: 14 }
            TableViewColumn { role: "detail"; title: "DETAIL"; width: 0 }
        }
    }
    Shortcut { sequence: keyId("autodb.audit.filter"); onActivated: App.auditFilter() }
    Shortcut { sequence: keyId("autodb.audit.clearFilter"); onActivated: App.auditClearFilter() }
    Shortcut { sequence: keyId("autodb.audit.nextPage"); onActivated: App.auditNextPage() }
    Shortcut { sequence: keyId("autodb.audit.previousPage"); onActivated: App.auditPrevPage() }
}
