// About.qml — what this build is and where its state lives. Also the splash
// shown once at start.

Dialog {
    title: "About autodb"
    standardButtons: Dialog.Ok
    helpText: "Enter or Esc to close"
    Text {
        wrapMode: Tui.WordWrap
        text: App.aboutText
    }
}
