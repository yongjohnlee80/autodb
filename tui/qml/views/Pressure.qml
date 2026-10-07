// A live, timestamped view of front-door capacity and refusals.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.pressure.title")
    dim: false
    standardButtons: Dialog.Close
    helpText: App.pressureAge
    onOpened: App.pressureOpened()
    onRejected: App.pressureClosed()
    TableView {
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        model: App.pressure
        TableViewColumn { role: "measure"; title: "MEASURE"; width: 28 }
        TableViewColumn { role: "value"; title: "VALUE"; width: 0 }
        delegate: Text {
            text: model.display
            color: model.state === "raised" ? Theme.pressureRaisedText : palette.text
        }
    }
}
