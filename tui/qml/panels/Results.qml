// Results.qml — what the last run returned.
//
// A summary line always on top — "SELECT ok — 12 row(s) in 38ms" — and below
// it the rows as a table or, with SPC j, as JSON, or the hint when nothing has
// run. The table's COLUMNS are the query's: App.results is a table model whose
// columns the result sets, not this file.
//
// Whenever the JSON isn't shown the table is there, empty under the hint when nothing
// has run: the results pane takes the keyboard (SPC t, Ctrl+j) whether or not there are rows.

Flex {
    direction: Tui.Vertical
    Text {
        visible: App.resultsEmpty
        text: qsTrId("autodb.results.text")
    }
    // a cursor on cells, sortable (s), its rows filtered by a WHERE (golib's DataTable); index is
    // the result's row, however it is sorted
    TableView {
        id: resultsTable
        visible: App.resultsTable
        cellCursor: true
        sortable: true
        model: App.results
        currentIndex: App.resultsIndex
        Layout.fillHeight: true
        onCurrentIndexChanged: App.resultsMoved(index)
        onActivated: App.inspectResult(index)
    }
}
