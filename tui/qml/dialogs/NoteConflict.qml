// NoteConflict.qml — the note changed on disk since it was opened: overwrite
// it, save yours as a new note, or keep editing. Keep takes initial focus so
// Enter cannot overwrite the disk copy by accident.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "note changed on disk"
    Text { wrapMode: Tui.WordWrap; text: App.conflictQuestion }
    DialogButtonBox {
        Button { text: "&Keep";      DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.conflict("keep") }
        Button { text: "&Overwrite"; DialogButtonBox.buttonRole: DialogButtonBox.DestructiveRole; onClicked: App.conflict("overwrite") }
        Button { text: "&Save as…";  DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.conflict("saveas") }
    }
}
