// The whole value, including lines hidden by the table's compact rendering.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.valueTitle
    dim: false
    helpText: qsTrId("autodb.value.helpText")
    Editor {
        palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText
        readOnly: true; wrap: true; text: App.valueText
        // A history script in its connection's SQL; "" for a cell's value.
        SyntaxHighlighter { definition: App.valueSyntax }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.value.text"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyValue() }
        Button { text: qsTrId("autodb.value.text2"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
