// ConnPicker.qml — SPC C: which connection this query runs on.

Dialog {
    title: "connection for this query"
    standardButtons: Dialog.Cancel
    ListView {
        model: App.connections
        textRole: "label"
        currentIndex: App.activeConnection
        onActivated: App.chooseConnection(index)
    }
}
