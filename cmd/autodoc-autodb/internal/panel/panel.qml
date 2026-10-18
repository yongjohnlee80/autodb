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
                    onActivated: App.use(index); onExpanded: App.opened(index) } }
            Tab { title: "NOTES"; Text { text: App.notes } }
            Tab { title: "REMOTE"; Text { text: App.remote } }
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

    Dialog { id: install; title: "Install AutoDB"
        standardButtons: Dialog.Ok | Dialog.Cancel; defaultButton: Dialog.Ok
        onAccepted: App.install()
        Text { text: App.installNote } }
}
