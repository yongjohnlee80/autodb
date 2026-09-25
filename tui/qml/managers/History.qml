// History.qml — what ran: when, who, where, how it ended. Enter shows the
// script; e loads it into the query; y copies it.

Dialog {
    title: "history"
    standardButtons: Dialog.Close
    helpText: "Enter script · e load · y copy"
    onOpened: App.historyOpened()
    TableView {
        id: table
        model: App.historyRows
        onActivated: App.historyShow(index)
        TableViewColumn { role: "when";   title: "WHEN";       width: 17 }
        TableViewColumn { role: "who";    title: "WHO";        width: 10 }
        TableViewColumn { role: "conn";   title: "CONNECTION"; width: 14 }
        TableViewColumn { role: "status"; title: "STATUS";     width: 9 }
        TableViewColumn { role: "rows";   title: "ROWS";       width: 7 }
        TableViewColumn { role: "took";   title: "TOOK";       width: 7 }
        TableViewColumn { role: "script"; title: "SCRIPT";     width: 0 }
    }
    Shortcut { sequence: "e"; onActivated: App.historyLoad(table.currentIndex) }
    Shortcut { sequence: "y"; onActivated: App.historyCopy(table.currentIndex) }
}
