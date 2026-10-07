// RemoteConnect.qml — Remote › Connect: a remote autodb server, and the
// autodb passphrase for its user.
//
// The passphrase is asked once: it opens this machine's device key and then
// signs in. On the first connect from this machine there is no key yet to
// check it against, so it is asked twice, with the warning that a wrong one
// counts against this network (App.remoteFirst, App.remoteWarning). A refused
// answer opens it again with the reason on its help line
// (App.remoteConnectError); cancelling after a failed connect goes back to
// the local daemon.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.remoteConnect.title")
    width: 80
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok          // Enter connects, from the last field too
    dim: true
    helpText: App.remoteConnectError
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.remoteConnect.text") }
        ComboBox {
            id: profile
            model: App.remoteProfiles
            textRole: "name"
            valueRole: "key"
            currentIndex: App.remoteProfile
            onActivated: App.remoteChoose(index)
        }
        Text { text: qsTrId("autodb.remoteConnect.text2") }
        TextField { id: passphrase; echoMode: TextInput.Password }
        Text { visible: App.remoteFirst; text: qsTrId("autodb.remoteConnect.text3") }
        TextField { id: again; visible: App.remoteFirst; echoMode: TextInput.Password }
        Text { visible: App.remoteFirst; wrapMode: Tui.WordWrap; text: App.remoteWarning }
    }
    onOpened: { passphrase.clear(); again.clear() }
    onAccepted: App.remoteConnect(profile.currentValue, passphrase.text, again.text)
    onRejected: App.remoteConnectCancelled()
}
