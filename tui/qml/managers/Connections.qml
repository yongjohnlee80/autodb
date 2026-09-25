// Connections.qml — the connections you can use: add, edit, test, delete,
// attach to a workspace.
//
// NEW: the actions are BUTTONS with their letters, beside the table, instead
// of letters you can only learn from a hint line. Tab moves table → buttons.
// A row's PROXY / PROFILE show what its edit form will start from.

Dialog {
    title: "connections"
    standardButtons: Dialog.Close
    helpText: App.connectionsStatus
    onOpened: App.connectionsOpened()
    Flex {
        direction: Tui.Vertical
        TableView {
            id: table
            model: App.connectionRows
            TableViewColumn { role: "id";      title: "ID";        width: 5 }
            TableViewColumn { role: "name";    title: "NAME";      width: 18 }
            TableViewColumn { role: "engine";  title: "ENGINE";    width: 9 }
            TableViewColumn { role: "profile"; title: "PROFILE";   width: 10 }
            TableViewColumn { role: "proxy";   title: "PROXY";     width: 8 }
            TableViewColumn { role: "target";  title: "TARGET DB"; width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&Add";    onClicked: App.connectionAdd() }
            Button { label: "&Edit";   onClicked: App.connectionEdit(table.currentIndex) }
            Button { label: "&Test";   onClicked: App.connectionTest(table.currentIndex) }
            Button { label: "&Delete"; onClicked: App.connectionDelete(table.currentIndex) }
            Button { label: "Attach to &workspace"; onClicked: App.connectionAttach(table.currentIndex) }
        }
    }
}
