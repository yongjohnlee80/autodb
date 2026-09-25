// Keyslot.qml — (admin) the service keyslot: its state, enable, remove.

Dialog {
    title: "service keyslot"
    standardButtons: Dialog.Close
    Flex {
        direction: Tui.Vertical
        Text { wrapMode: Tui.WordWrap; text: App.keyslotText }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&Enable"; enabled: App.keyslotCanEnable; onClicked: App.keyslotEnable() }
            Button { label: "Remo&ve"; enabled: App.keyslotCanRemove; onClicked: App.keyslotRemove() }
        }
    }
}
