// Results.qml — what the last run returned.
//
// NEW: a summary line always on top — "12 rows · 38 ms · prod-pg" or
// "UPDATE ok — 3 rows affected" — instead of the summary replacing the grid.
// Below it, the grid or the JSON (SPC j switches), or the empty-state hint.
// The grid's COLUMNS come from the result: App.results is a table model whose
// columns are the query's (a runtime schema, not declared here).

Frame {
    title: "results"
    Flex {
        direction: Tui.Vertical
        Text { text: App.resultsSummary }
        TableView {
            visible: App.resultsAsTable
            model: App.results
            // Enter or v inspects the row.
            onActivated: App.inspectRow(index)
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
