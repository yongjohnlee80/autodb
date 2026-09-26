// CIDRs added to the account remain after this one token is revoked.
Dialog {
    title: "allowlist widening"
    dim: false
    Editor { readOnly: true; text: App.widenText }
    DialogButtonBox {
        Button { text: "&Add and mint"; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: "&Cancel"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onAccepted: App.widenAccepted()
    onRejected: App.widenCancelled()
}
