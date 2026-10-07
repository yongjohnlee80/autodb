// Connections.qml — the connections you can use: add, edit, test, delete,
// attach to a workspace.
//
// The actions are the dialog's own buttons, ActionRole as in Qt: each acts on
// the table and the dialog stays; their letters press them, Tab moves table →
// buttons, and Close is the one that answers. A row's PROXY and PROFILE are the
// two decisions most often confused: whether the front door carries it, and
// what SQL a client may send over it.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.connections.title")
    dim: false
    helpText: App.connectionsStatus
    // A column, as Qt's ColumnLayout in a Dialog: the table sizes to its rows.
    Flex {
        direction: Tui.Vertical
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.connectionRows
            TableViewColumn { role: "id";      title: "ID";        width: 5 }
            TableViewColumn { role: "name";    title: "NAME";      width: 18 }
            TableViewColumn { role: "engine";  title: "ENGINE";    width: 9 }
            TableViewColumn { role: "profile"; title: "PROFILE";   width: 10 }
            TableViewColumn { role: "proxy";   title: "PROXY";     width: 7 }
            TableViewColumn { role: "target";  title: "TARGET DB"; width: 0 }
        }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.connections.text");    DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.connectionAdd() }
        Button { text: qsTrId("autodb.connections.text2");   DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.connectionEdit(table.currentIndex) }
        Button { text: qsTrId("autodb.connections.text3");   DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.connectionTest(table.currentIndex) }
        Button { text: qsTrId("autodb.connections.text4"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.connectionDelete(table.currentIndex) }
        Button { text: qsTrId("autodb.connections.text5"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.connectionAttach(table.currentIndex) }
        Button { text: qsTrId("autodb.connections.text6");  DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
