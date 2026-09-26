// About.qml — what this build is and where its state lives.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "About autodb"
    dim: false        // the backdrop fades only for sign-in and quitting
    standardButtons: Dialog.Ok
    defaultButton: Dialog.Ok          // Enter closes it, as its help line says
    helpText: "q, Enter or Esc closes"
    Text { wrapMode: Tui.WordWrap; text: App.aboutText }
}
