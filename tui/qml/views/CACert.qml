// The public CA document itself, or an explicit system-roots explanation.
Dialog {
    title: App.caTitle
    dim: false
    helpText: App.caStatus
    onRejected: App.caClosed()
    Editor { readOnly: true; text: App.caText }
    DialogButtonBox {
        Button { text: "&Copy certificate"; enabled: App.caCanCopy; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyCA() }
        Button { text: "&Done"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
