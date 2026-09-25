// Results.qml — what the last run returned.
//
// A summary line always on top — "SELECT ok — 12 row(s) in 38ms" — and below
// it the rows as a table or, with SPC j, as JSON, or the hint when nothing has
// run. The table's COLUMNS are the query's: App.results is a table model whose
// columns the result sets, not this file.

Frame {
    title: "results"
    Flex {
        direction: Tui.Vertical
        Text { text: App.resultsSummary }
        TableView {
            visible: App.resultsAsTable
            model: App.results
        }
        Editor {
            visible: App.resultsAsJSON
            readOnly: true
            text: App.resultsJSON
        }
        Text {
            visible: App.resultsEmpty
            text: "no results — SPC r runs the query"
        }
    }
}
