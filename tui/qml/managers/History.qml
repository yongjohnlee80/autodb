// Recorded query executions. The host owns selection, loading and copying.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.history.title")
    dim: false
    standardButtons: Dialog.Close
    helpText: App.historyStatus
    onRejected: App.historyClosed()
    TableView {
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        id: table
        model: App.historyRows
        onActivated: App.historyShow(index)
        TableViewColumn { role: "when"; title: "WHEN"; width: 17 }
        TableViewColumn { role: "who"; title: "WHO"; width: 10 }
        TableViewColumn { role: "conn"; title: "CONNECTION"; width: 14 }
        TableViewColumn { role: "status"; title: "STATUS"; width: 9 }
        TableViewColumn { role: "rows"; title: "ROWS"; width: 7 }
        TableViewColumn { role: "took"; title: "TOOK"; width: 7 }
        TableViewColumn { role: "script"; title: "SCRIPT"; width: 0 }
    }
    Shortcut { sequence: "e"; onActivated: App.historyLoad(table.currentIndex) }
    Shortcut { sequence: "y"; onActivated: App.historyCopy(table.currentIndex) }
    Shortcut { sequence: "f"; onActivated: App.historyFilter() }
    Shortcut { sequence: "x"; onActivated: App.historyClearFilter() }
    Shortcut { sequence: "n"; onActivated: App.historyNextPage() }
    Shortcut { sequence: "p"; onActivated: App.historyPrevPage() }
}
