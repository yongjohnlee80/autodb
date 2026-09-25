// UnsavedNote.qml — the note has unsaved changes: save, discard, or stay.
// Three answers, none the default: an Enter must not throw work away.

Dialog {
    title: "unsaved note"
    defaultButton: Dialog.NoButton
    Text { wrapMode: Tui.WordWrap; text: App.unsavedQuestion }
    DialogButtonBox {
        Button { text: "&Save";    DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.unsaved("save") }
        Button { text: "&Discard"; DialogButtonBox.buttonRole: DialogButtonBox.DestructiveRole; onClicked: App.unsaved("discard") }
        Button { text: "S&tay";    DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.unsaved("stay") }
    }
}
