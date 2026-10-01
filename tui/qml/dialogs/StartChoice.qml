// StartChoice.qml — [tui] start = "ask": where the program starts. This
// computer's daemon, or one of the remote servers in remotes.toml; closing it
// starts connected to nothing.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "start on"
    width: 80
    dim: true
    helpText: "Enter starts there · Esc starts connected to nothing"
    ListView {
        id: choices
        model: App.startChoices
        textRole: "label"
        focus: true
        onActivated: App.startChosen(index)
    }
    onRejected: App.startDeclined()
}
