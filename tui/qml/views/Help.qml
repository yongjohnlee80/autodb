// Help.qml — every command and key, generated from the catalog, so it cannot
// drift from what the leader menu and the menu bar do.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "help"
    dim: false        // the backdrop fades only for sign-in and quitting
    standardButtons: Dialog.Close
    defaultButton: Dialog.Close       // Enter closes it, as its help line says
    helpText: "q, Esc or Enter closes"
    Editor {
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        readOnly: true
        wrap: true
        text: App.helpText
    }
}
