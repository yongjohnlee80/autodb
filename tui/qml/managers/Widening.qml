// CIDRs added to the account remain after this one token is revoked.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "allowlist widening"
    dim: false
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.widenText }
    DialogButtonBox {
        Button { text: "&Add and mint"; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: "&Cancel"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onAccepted: App.widenAccepted()
    onRejected: App.widenCancelled()
}
