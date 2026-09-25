// Widening.qml — the token's IPs are outside your allowlist: adding them is a
// standing change with four consequences, named here. No default answer.

Dialog {
    title: "widen your allowlist?"
    defaultButton: Dialog.NoButton
    Text { wrapMode: Tui.WordWrap; text: App.wideningText }
    DialogButtonBox {
        Button { text: "&Cancel"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.widening(false) }
        Button { text: "&Add them and mint"; DialogButtonBox.buttonRole: DialogButtonBox.DestructiveRole; onClicked: App.widening(true) }
    }
}
