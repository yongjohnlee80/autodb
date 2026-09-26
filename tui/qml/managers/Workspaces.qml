// A workspace and its attached connections are two views over one server list.
Dialog {
    title: "workspaces"
    dim: false
    helpText: App.workspacesStatus
    onRejected: App.workspaceManagerClosed()
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
                    currentIndex: App.workspaceIndex
                    onCurrentIndexChanged: App.workspaceChosen(index)
                    TableViewColumn { role: "id"; title: "ID"; width: 5 }
                    TableViewColumn { role: "name"; title: "NAME"; width: 0 }
                    TableViewColumn { role: "conns"; title: "CONNS"; width: 6 }
                }
            }
            Frame {
                title: "connections here"
                TableView {
                    id: attached
                    model: App.attachedRows
                    TableViewColumn { role: "name"; title: "NAME"; width: 0 }
                    TableViewColumn { role: "engine"; title: "ENGINE"; width: 9 }
                }
            }
        }
    }
    DialogButtonBox {
        Button { text: "&New"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceNew() }
        Button { text: "&Rename"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceRename(spaces.currentIndex) }
        Button { text: "&Delete"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDelete(spaces.currentIndex) }
        Button { text: "&Attach"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceAttach(spaces.currentIndex) }
        Button { text: "De&tach"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDetach(spaces.currentIndex, attached.currentIndex) }
        Button { text: "&Close"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
