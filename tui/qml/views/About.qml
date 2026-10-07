// About.qml — what this build is and where its state lives.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.about.title")
    dim: false        // the backdrop fades only for sign-in and quitting
    standardButtons: Dialog.Ok
    defaultButton: Dialog.Ok          // Enter closes it, as its help line says
    helpText: qsTrId("autodb.about.helpText")
    Text { wrapMode: Tui.WordWrap; text: App.aboutText }
}
