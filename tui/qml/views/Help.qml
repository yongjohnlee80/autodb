// Help.qml — every command and key, generated from the catalog, so it cannot
// drift from what the leader menu and the menu bar do.

Dialog {
    title: "help"
    dim: false        // the backdrop fades only for sign-in and quitting
    standardButtons: Dialog.Close
    defaultButton: Dialog.Close       // Enter closes it, as its help line says
    helpText: "Esc or Enter closes"
    Editor {
        readOnly: true
        wrap: true
        text: App.helpText
    }
}
