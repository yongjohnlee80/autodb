// About.qml — what this build is and where its state lives.

Dialog {
    title: "About autodb"
    dim: false        // the backdrop fades only for sign-in and quitting
    standardButtons: Dialog.Ok
    defaultButton: Dialog.Ok          // Enter closes it, as its help line says
    helpText: "Enter or Esc closes"
    Text { wrapMode: Tui.WordWrap; text: App.aboutText }
}
