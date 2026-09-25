// Profile.qml — who you are, and changing your passphrase.

Dialog {
    title: "profile"
    standardButtons: Dialog.Close
    Flex {
        direction: Tui.Vertical
        Text { text: App.profileText }
        Text { text: "current passphrase" }
        TextField { id: current; echoMode: TextInput.Password }
        Text { text: "new passphrase" }
        TextField { id: next; echoMode: TextInput.Password }
        Text { text: "again" }
        TextField { id: again; echoMode: TextInput.Password }
        Button { text: "&Change passphrase"; onClicked: App.changePassphrase(current.text, next.text, again.text) }
        Text { text: App.profileError }
    }
}
