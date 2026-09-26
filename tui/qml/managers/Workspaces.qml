// A workspace and its attached connections are two views over one server list.
Dialog {
    closeOnQ: true
    title: "workspaces"
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
                title: "workspaces"
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
                title: "connections here"
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
        Button { id: newWorkspaceButton; text: "&New"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceNew() }
        Button { text: "&Rename"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceRename(spaces.currentIndex) }
        Button { text: "&Delete"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDelete(spaces.currentIndex) }
        Button { text: "&Attach"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceAttach(spaces.currentIndex) }
        Button { text: "De&tach"; enabled: App.workspaceCanManage; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.workspaceDetach(spaces.currentIndex, attached.currentIndex) }
        Button { id: closeWorkspaceButton; text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
