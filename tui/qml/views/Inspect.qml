// One result row: full values behind compact one-line column summaries.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.inspect.title")
    dim: false
    standardButtons: Dialog.Close
    helpText: qsTrId("autodb.inspect.helpText")
    ListView {
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        id: columns
        model: App.inspectRows
        textRole: "line"
        onActivated: App.openValue(index)
    }
    Shortcut { sequence: "y"; onActivated: App.copyInspected(columns.currentIndex) }
}
