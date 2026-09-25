// Explorer.qml — workspaces, their connections and notes, and each
// connection's schema, as one tree.
//
// NEW: a filter field above the tree — typing narrows it (the old `/` search
// walked the tree one match at a time). The tree is a VIEW over App.explorer, a
// host model: workspaces → connections / notes → schema → tables / views /
// functions → columns / partitions, loaded as a branch is opened.

Frame {
    title: "explorer"
    Flex {
        direction: Tui.Vertical
        TextField {
            placeholderText: "filter…"
            onTextEdited: App.filterExplorer(text)
        }
        TreeView {
            model: App.explorer
            textRole: "label"
            badgeRole: "badge"
            // Enter on a table scaffolds a query; on a note, opens it; moving
            // the cursor retargets the query's connection.
            onActivated: App.explorerActivated(index)
            onCurrentIndexChanged: App.explorerMoved(index)
            onExpanded: App.explorerExpanded(index)
        }
    }
}
