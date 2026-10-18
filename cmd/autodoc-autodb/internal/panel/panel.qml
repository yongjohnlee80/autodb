import tui 1.0
import autodoc.plugin 1.0
import autodb 1.0

// The AutoDB panel: a tab per area, the ☰ menu of the tab's actions, and the dialogs. Every source
// is App.*, set by the plugin (panel.go); every action calls an App.* handler. Esc never reaches
// the panel (AutoDoc keeps it to hide the card), so each dialog has its own Cancel and the menu
// closes on a row or F10.
Window {
    palette.window: Theme.app.window
    palette.windowText: Theme.app.windowText

    Shortcut { sequence: "Alt+1"; onActivated: App.goTab(0) }
    Shortcut { sequence: "Alt+2"; onActivated: App.goTab(1) }
    Shortcut { sequence: "Alt+3"; onActivated: App.goTab(2) }
    Shortcut { sequence: "Alt+4"; onActivated: App.goTab(3) }

    MenuBar { Dock.edge: Tui.Top
        Menu { title: "☰ &Menu"
            Instantiator { model: App.menu
                MenuItem { text: model.label; enabled: model.enabled; onTriggered: App.menuRow(model.id) } } } }

    Flex { direction: Tui.Vertical
        Text { text: App.where }
        TabView { id: tabs; currentIndex: App.tabIndex; onCurrentIndexChanged: App.tab(index)
            Tab { title: "CONNECTIONS"
                TreeView { id: tree; focus: true; model: App.tree; textRole: "label"
                    onActivated: App.use(index); onExpanded: App.opened(index)
                    onCurrentIndexChanged: App.current(index) } }
            Tab { title: "NOTES"
                Flex { direction: Tui.Vertical
                    ComboBox { id: noteWs; model: App.noteWorkspaces; textRole: "label"; valueRole: "key"
                        placeholderText: "workspace"; onActivated: App.noteWorkspace(noteWs.currentValue) }
                    ListView { id: notes; model: App.notes; textRole: "label"
                        onActivated: App.noteOpen(index); onCurrentIndexChanged: App.noteCurrent(index) } } }
            Tab { title: "REMOTE"
                Flex { direction: Tui.Vertical
                    ListView { id: remotes; model: App.remotes; textRole: "label"
                        onActivated: App.remoteOpen(index); onCurrentIndexChanged: App.remoteCurrent(index) }
                    Text { text: App.remote } } }
            Tab { title: "SYSTEM"; Text { text: App.system } } }
        Text { text: App.status }
        Text { text: App.problem } }

    Dialog { id: signIn; title: "Sign in to AutoDB"
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.signIn(signInName.text, signInPass.text)
        onRejected: App.signInCancelled()
        Flex { direction: Tui.Vertical
            Text { text: App.signInNote }
            TextField { id: signInName; placeholderText: "name"; text: App.signInName }
            TextField { id: signInPass; placeholderText: "passphrase"; echoMode: TextInput.Password } } }

    Dialog { id: bootstrap; title: "Make the first AutoDB user"
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.bootstrap(bootName.text, bootPass.text, bootAgain.text)
        Flex { direction: Tui.Vertical
            Text { text: App.bootstrapNote }
            TextField { id: bootName; placeholderText: "name"; text: "root" }
            TextField { id: bootPass; placeholderText: "passphrase, 8 characters or more"; echoMode: TextInput.Password }
            TextField { id: bootAgain; placeholderText: "the passphrase again"; echoMode: TextInput.Password } } }

    Dialog { id: nameDialog; title: App.nameTitle
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.named(nameField.text)
        Flex { direction: Tui.Vertical
            Text { text: App.nameNote }
            TextField { id: nameField; placeholderText: "name"; text: App.nameValue } } }

    Dialog { id: confirm; title: App.confirmTitle
        standardButtons: Dialog.Yes | Dialog.No; defaultButton: Dialog.No
        onAccepted: App.confirmed()
        Text { text: App.confirmText } }

    Dialog { id: addConn; title: "Add connection"
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.addConnection(connEngine.currentValue, connName.text, connDSN.text, connCA.text, connCert.text, connKey.text)
        Flex { direction: Tui.Vertical
            Text { text: App.addConnNote }
            ComboBox { id: connEngine; model: App.engines; textRole: "label"; valueRole: "key"; placeholderText: "database type" }
            TextField { id: connName; placeholderText: "name" }
            TextField { id: connDSN; placeholderText: "DSN"; echoMode: TextInput.Password }
            TextField { id: connCA; placeholderText: "TLS: CA file (optional)" }
            TextField { id: connCert; placeholderText: "TLS: certificate file (optional)" }
            TextField { id: connKey; placeholderText: "TLS: key file (optional)" } } }

    Dialog { id: remoteDialog; title: App.remoteTitle
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.remoteSaved(remoteName.text, remoteHost.text, remotePort.text, remoteUser.text, remoteKey.text, remoteAuth.currentValue)
        Flex { direction: Tui.Vertical
            Text { text: App.remoteNote }
            TextField { id: remoteName; placeholderText: "name"; text: App.remoteName }
            TextField { id: remoteHost; placeholderText: "host"; text: App.remoteHost }
            TextField { id: remotePort; placeholderText: "port (7422)"; text: App.remotePort }
            TextField { id: remoteUser; placeholderText: "your AutoDB user there"; text: App.remoteUser }
            TextField { id: remoteKey; placeholderText: "SSH key (~/.ssh/id_ed25519)"; text: App.remoteKey }
            ComboBox { id: remoteAuth; model: App.remoteAuths; textRole: "label"; valueRole: "key"; currentIndex: App.remoteAuthIndex } } }

    Dialog { id: secret; title: App.secretTitle
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.secret(secretPass.text, secretAgain.text)
        onRejected: App.secretCancelled()
        Flex { direction: Tui.Vertical
            Text { text: App.secretNote }
            TextField { id: secretPass; placeholderText: "passphrase"; echoMode: TextInput.Password }
            TextField { id: secretAgain; placeholderText: "the passphrase again"; echoMode: TextInput.Password; visible: App.secretTwice } } }

    Dialog { id: install; title: "Install AutoDB"
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.install()
        Text { text: App.installNote } }
}
