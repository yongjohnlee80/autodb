// A live, timestamped view of front-door capacity and refusals.
Dialog {
    title: "pressure"
    dim: false
    standardButtons: Dialog.Close
    helpText: App.pressureAge
    onOpened: App.pressureOpened()
    onRejected: App.pressureClosed()
    TableView {
        model: App.pressure
        TableViewColumn { role: "measure"; title: "MEASURE"; width: 28 }
        TableViewColumn { role: "value"; title: "VALUE"; width: 0 }
        delegate: Text {
            text: model.display
            color: model.state === "raised" ? Theme.pressureRaisedText : palette.text
        }
    }
}
