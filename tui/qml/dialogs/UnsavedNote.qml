// UnsavedNote.qml — the note has unsaved changes: save, discard, or stay.
// Save takes initial focus: Enter saves, while Discard requires an explicit
// mnemonic, focus move, or click, so a stray Enter cannot throw work away.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.unsavedNote.title")
    Text { wrapMode: Tui.WordWrap; text: App.unsavedQuestion }
    DialogButtonBox {
        Button { text: qsTrId("autodb.unsavedNote.text");    DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.unsaved("save") }
        Button { text: qsTrId("autodb.unsavedNote.text2"); DialogButtonBox.buttonRole: DialogButtonBox.DestructiveRole; onClicked: App.unsaved("discard") }
        Button { text: qsTrId("autodb.unsavedNote.text3");    DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.unsaved("stay") }
    }
}
