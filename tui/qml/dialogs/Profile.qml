// Self-service account detail and passphrase change; fields never enter App state.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "profile"
    width: 72
    dim: false
    helpText: App.profileError
    Flex {
        direction: Tui.Vertical
        Text { wrapMode: Tui.WordWrap; text: App.profileText }
        Text { text: "current passphrase" }
        TextField { id: current; echoMode: TextInput.Password }
        Text { text: "new passphrase" }
        TextField { id: next; echoMode: TextInput.Password }
        Text { text: "again" }
        TextField { id: again; echoMode: TextInput.Password }
    }
    DialogButtonBox {
        Button { text: "&Update passphrase"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { App.changePassphrase(current.text, next.text, again.text)
                              current.clear(); next.clear(); again.clear() } }
        Button { text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onOpened: { current.clear(); next.clear(); again.clear() }
    onRejected: { current.clear(); next.clear(); again.clear(); App.profileClosed() }
}
