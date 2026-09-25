// NoteConflict.qml — the note changed on disk since it was opened.

Dialog {
    title: "note changed on disk"
    defaultButton: Dialog.NoButton
    Text { wrapMode: Tui.WordWrap; text: App.conflictQuestion }
    DialogButtonBox {
        Button { text: "&Overwrite"; DialogButtonBox.buttonRole: DialogButtonBox.DestructiveRole; onClicked: App.conflict("overwrite") }
        Button { text: "&Save as…";  DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.conflict("saveas") }
        Button { text: "&Keep";      DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.conflict("keep") }
    }
}
