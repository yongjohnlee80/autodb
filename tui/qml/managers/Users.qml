// Users.qml — (admin) users: add, role, passphrase reset, enable/disable,
// remove, grant on a connection, allowed IPs.

Dialog {
    title: "users"
    standardButtons: Dialog.Close
    helpText: App.usersStatus
    onOpened: App.usersOpened()
    Flex {
        direction: Tui.Vertical
        TableView {
            id: table
            model: App.userRows
            TableViewColumn { role: "id";    title: "ID";    width: 5 }
            TableViewColumn { role: "name";  title: "NAME";  width: 20 }
            TableViewColumn { role: "role";  title: "ROLE";  width: 10 }
            TableViewColumn { role: "state"; title: "STATE"; width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&Add";          onClicked: App.userAdd() }
            Button { label: "&Role";         onClicked: App.userRole(table.currentIndex) }
            Button { label: "&Passphrase";   onClicked: App.userResetPassphrase(table.currentIndex) }
            Button { label: "Enable/disable"; onClicked: App.userToggle(table.currentIndex) }
            Button { label: "&Grant";        onClicked: App.userGrant(table.currentIndex) }
            Button { label: "&IPs";          onClicked: App.userIPs(table.currentIndex) }
            Button { label: "Remo&ve";       onClicked: App.userRemove(table.currentIndex) }
        }
    }
}
