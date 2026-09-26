// The token is bound to one offered connection. The backend revalidates it.
Dialog {
    title: "create access token"
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.tokenFormError
    onRejected: App.tokenFormClosed()
    Flex {
        direction: Tui.Vertical
        Text { text: "name" }
        TextField { id: name; text: App.tokenFormName }
        Text { text: "expires in days (1–365, blank for default)" }
        TextField { id: days; text: App.tokenFormDays }
        Text { text: "IPs (comma-separated CIDRs, blank inherits your allowlist)" }
        TextField { id: ips; text: App.tokenFormIPs }
        Text { text: "connection" }
        ComboBox { id: conn; model: App.tokenConnections; textRole: "label"; valueRole: "id"; currentIndex: App.tokenConnectionIndex }
        Text { text: "allow cleartext debug traffic? (yes/no)"; visible: App.tokenFormCleartext }
        TextField { id: cleartext; text: App.tokenFormDebug; visible: App.tokenFormCleartext; placeholderText: "no" }
    }
    onAccepted: App.mintToken(name.text, days.text, ips.text, conn.currentValue, cleartext.text)
}
