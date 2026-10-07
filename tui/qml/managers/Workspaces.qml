// A workspace and its attached connections are two views over one server list.
Dialog {
    closeOnQ: true
    title: qsTrId("autodb.workspaces.title")
    dim: false
    maxWidthPercent: 75
    maxHeightPercent: 75
    helpText: App.workspacesStatus
    onRejected: App.workspaceManagerClosed()
    Flex {
        direction: Tui.Vertical
        Split {
            orientation: Tui.Horizontal
            ratio: 0.55
            Frame {
                title: qsTrId("autodb.workspaces.title2")
                TableView {
                    palette.highlight: Theme.document.highlight
                    palette.highlightedText: Theme.document.highlightedText
                    id: spaces
                    model: App.workspaceRows
                    currentIndex: App.workspaceIndex
                    onCurrentIndexChanged: App.workspaceChosen(index)
                    TableViewColumn { role: "id"; title: "ID"; width: 5 }
                    TableViewColumn { role: "name"; title: "NAME"; width: 0 }
                    TableViewColumn { role: "conns"; title: "CONNS"; width: 6 }
                }
            }
            Frame {
                title: qsTrId("autodb.workspaces.title3")
                TableView {
                    palette.highlight: Theme.document.highlight
                    palette.highlightedText: Theme.document.highlightedText
                    id: attached
                    model: App.attachedRows
                    TableViewColumn { role: "name"; title: "NAME"; width: 0 }
                    TableViewColumn { role: "engine"; title: "ENGINE"; width: 9 }
                }
            }
        }
    }
    DialogButtonBox {
        Button { id: newWorkspaceButton; text: qsTrId("autodb.workspaces.text"); enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceNew() }
        Button { text: qsTrId("autodb.workspaces.text2"); enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceRename(spaces.currentIndex) }
        Button { text: qsTrId("autodb.workspaces.text3"); enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDelete(spaces.currentIndex) }
        Button { text: qsTrId("autodb.workspaces.text4"); enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceAttach(spaces.currentIndex) }
        Button { text: qsTrId("autodb.workspaces.text5"); enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDetach(spaces.currentIndex, attached.currentIndex) }
        Button { id: closeWorkspaceButton; text: qsTrId("autodb.workspaces.text6"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
