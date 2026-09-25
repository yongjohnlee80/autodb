// ConnCard.qml — how to reach a connection through the front door: DSN, JDBC,
// token, TLS, and the limits it will meet. The warning comes before the
// token. v/V then y copies a selection; Y copies all.

Dialog {
    title: App.cardTitle
    standardButtons: Dialog.Close
    helpText: "v/V select · y copy · Y copy all · q close"
    Editor {
        readOnly: true
        text: App.cardText
    }
    Shortcut { sequence: "Shift+Y"; onActivated: App.copyCard() }
}
