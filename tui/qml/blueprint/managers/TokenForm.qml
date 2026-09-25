// TokenForm.qml — mint a token: its name, lifetime, the IPs it may come from,
// and the connection it opens. The debug-cleartext question appears only for
// an admin on a door known to be cleartext.

Dialog {
    title: App.tokenFormTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    helpText: App.tokenFormError
    Flex {
        direction: Tui.Vertical
        Text { text: "name" }
        TextField { id: name }
        Text { text: "expires in (days)" }
        TextField { id: days; text: "30" }
        Text { text: "IPs (comma-separated CIDRs)" }
        TextField { id: ips }
        Text { text: "connection" }
        ComboBox { id: conn; model: App.connections; textRole: "label"; valueRole: "id" }
        Text { text: "allow cleartext (debug)"; visible: App.tokenFormCleartext }
        ComboBox { id: cleartext; visible: App.tokenFormCleartext; model: App.yesNo; textRole: "label"; valueRole: "id" }
    }
    onAccepted: App.mintToken(name.text, days.text, ips.text, conn.currentValue, cleartext.currentValue)
}
