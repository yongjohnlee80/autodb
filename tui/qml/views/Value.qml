// The whole value, including lines hidden by the table's compact rendering.
Dialog {
    title: App.valueTitle
    dim: false
    helpText: "Tab to Copy · Esc close"
    Editor { readOnly: true; wrap: true; text: App.valueText }
    DialogButtonBox {
        Button { text: "&Copy"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyValue() }
        Button { text: "C&lose"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
