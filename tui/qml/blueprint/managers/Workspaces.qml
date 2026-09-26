// Workspaces.qml — workspaces, and the connections attached to each.
//
// Two tables side by side; the buttons act on the one in use. Deletion
// explicitly unlinks every attached connection at commit (Johno, 2026-09-26),
// while retaining the connection records and local notes.

Dialog {
    title: "workspaces"
    standardButtons: Dialog.Close
    helpText: App.workspacesStatus
    Flex {
        direction: Tui.Vertical
        Split {
            orientation: Tui.Horizontal
            ratio: 0.55
            Frame {
                title: "workspaces"
                TableView {
                    id: spaces
                    model: App.workspaceRows
                    onCurrentIndexChanged: App.workspaceChosen(index)
                    TableViewColumn { role: "id";    title: "ID";    width: 5 }
                    TableViewColumn { role: "name";  title: "NAME";  width: 0 }
                    TableViewColumn { role: "conns"; title: "CONNS"; width: 6 }
                }
            }
            Frame {
                title: "connections"
                TableView {
                    id: attached
                    model: App.attachedRows
                    TableViewColumn { role: "name";   title: "NAME";   width: 0 }
                    TableViewColumn { role: "engine"; title: "ENGINE"; width: 9 }
                }
            }
        }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&New workspace"; onClicked: App.workspaceNew() }
            Button { label: "&Rename";        onClicked: App.workspaceRename(spaces.currentIndex) }
            Button { label: "&Delete";        onClicked: App.workspaceDelete(spaces.currentIndex) }
            Button { label: "&Attach";        onClicked: App.workspaceAttach(spaces.currentIndex) }
            Button { label: "De&tach";        onClicked: App.workspaceDetach(spaces.currentIndex, attached.currentIndex) }
        }
    }
}
