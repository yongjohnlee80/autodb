// Value.qml — one value, whole: a long text, JSON, a secret shown once.

Dialog {
    title: App.valueTitle
    standardButtons: Dialog.Close
    helpText: "y copy · Esc close"
    Editor {
        readOnly: true
        wrap: true
        text: App.valueText
    }
    Shortcut { sequence: "y"; onActivated: App.copyValue() }
}
