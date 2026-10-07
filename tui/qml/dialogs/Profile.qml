// Self-service account detail and passphrase change; fields never enter App state.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.profile.title")
    width: 72
    dim: false
    helpText: App.profileError
    Flex {
        direction: Tui.Vertical
        Text { wrapMode: Tui.WordWrap; text: App.profileText }
        Text { text: qsTrId("autodb.profile.text") }
        TextField { id: current; echoMode: TextInput.Password }
        Text { text: qsTrId("autodb.profile.text2") }
        TextField { id: next; echoMode: TextInput.Password }
        Text { text: qsTrId("autodb.profile.text3") }
        TextField { id: again; echoMode: TextInput.Password }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.profile.text4"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { App.changePassphrase(current.text, next.text, again.text)
                              current.clear(); next.clear(); again.clear() } }
        Button { text: qsTrId("autodb.profile.text5"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onOpened: { current.clear(); next.clear(); again.clear() }
    onRejected: { current.clear(); next.clear(); again.clear(); App.profileClosed() }
}
