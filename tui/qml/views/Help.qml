// Help.qml — every command and key, generated from the catalog, so it cannot
// drift from what the leader menu and the menu bar do.

Dialog {
    title: "help"
    standardButtons: Dialog.Close
    helpText: "Esc or Enter closes"
    Editor {
        readOnly: true
        wrap: true
        text: App.helpText
    }
}
