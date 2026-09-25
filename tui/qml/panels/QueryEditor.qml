// QueryEditor.qml — the query buffer.
//
// Its title names where the query will run ("query → prod-pg", or "no
// connection — SPC C selects one"); vim or TextEdit keys, as Options › Editor
// chose. SQL highlighting follows when the SQL highlighter exists upstream.

Frame {
    title: App.queryTitle
    Editor {
        id: editor
        focus: true
        keyset: App.keyset
        text: App.queryText
        onTextChanged: App.queryEdited()
        onModeChanged: App.editorModeChanged()
    }
}
