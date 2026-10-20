// main.qml — autodb's screen, in QML.
//
// STRUCTURE ONLY: what is on the screen, where it is docked, and what each
// control triggers. It names no colour — the imported theme does — and it runs
// nothing itself: a command is App.run(<command id>), through the one catalog
// that decides what is offered to whom; the menu bar and the leader menu are
// views of that catalog (App.menu, App.leader).
//
// A palette is set ONCE, where it starts: the Window carries the application
// palette; a surface that is a distinct part of the design overrides only its
// own roles; everything else inherits.

import tui 1.0
import autodb 1.0                 // App: this program's state and commands
import autodb.theme.dark 1.0      // the Theme singleton — Options › Theme switches it
import autodb.views 1.0           // Leader, Hints, Help, About
import autodb.dialogs 1.0         // ConfirmQuit, Login, Bootstrap, the note dialogs, ConnPicker
import autodb.panels 1.0          // Results
import autodb.managers 1.0        // Connections, ConnectionForm, Attach
import tui.panels 1.0             // Panel: the explorer and the results (golib)

Window {
    palette.window: Theme.app.window
    palette.windowText: Theme.app.windowText
    palette.button: Theme.app.button
    palette.buttonText: Theme.app.buttonText
    palette.highlight: Theme.app.highlight
    palette.highlightedText: Theme.app.highlightedText
    palette.base: Theme.app.base
    palette.text: Theme.app.text
    palette.inactive.highlight: Theme.app.inactive.highlight
    palette.inactive.highlightedText: Theme.app.inactive.highlightedText
    palette.mid: Theme.app.mid
    palette.light: Theme.app.light

    // KSyntaxHighlighting's styles, set once: every highlighter under the
    // Window wears them — the query editor, and a history script's view.
    syntax.keyword: Theme.syntax.keyword
    syntax.controlFlow: Theme.syntax.controlFlow
    syntax.dataType: Theme.syntax.dataType
    syntax.attribute: Theme.syntax.attribute
    syntax.function: Theme.syntax.function
    syntax.string: Theme.syntax.string
    syntax.specialChar: Theme.syntax.specialChar
    syntax.decVal: Theme.syntax.decVal
    syntax.float: Theme.syntax.float
    syntax.baseN: Theme.syntax.baseN
    syntax.constant: Theme.syntax.constant
    syntax.comment: Theme.syntax.comment
    syntax.alert: Theme.syntax.alert
    syntax.import: Theme.syntax.import
    syntax.operator: Theme.syntax.operator

    // ---- keys ------------------------------------------------------------
    //
    // A Shortcut sees only what the focused widget leaves, and none is live
    // while a dialog is open — the dialog has the keyboard.
    // Each names its key command (tui/keys.go), which a user's keymap.xml may move.
    Shortcut { sequence: keyId("autodb.window.leader"); onActivated: leader.open() }
    Shortcut { sequence: keyId("autodb.window.quit"); onActivated: App.run("app.quit") }
    Shortcut { sequence: keyId("autodb.window.hints"); onActivated: App.showHints() }
    Shortcut { sequence: keyId("autodb.window.search"); onActivated: App.openSearch() }
    Shortcut { sequence: keyId("autodb.window.searchNext"); onActivated: App.searchNext() }
    Shortcut { sequence: keyId("autodb.window.searchPrevious"); onActivated: App.searchPrevious() }
    // Between the panes: Ctrl+h/j/k/l. There is no Alt+h/j/k/l: Alt+letter
    // opens the menu bar's menus, and Alt+H would take Home's.
    Shortcut { sequence: keyId("autodb.window.movePane.h"); onActivated: App.movePane("h") }
    Shortcut { sequence: keyId("autodb.window.movePane.j"); onActivated: App.movePane("j") }
    Shortcut { sequence: keyId("autodb.window.movePane.k"); onActivated: App.movePane("k") }
    Shortcut { sequence: keyId("autodb.window.movePane.l"); onActivated: App.movePane("l") }

    // ---- the menu bar: a view of the catalog -------------------------------
    //
    // App.menu is the catalog projected for who is signed in and what state
    // the program is in: its top-level menus, each with rows that are items,
    // radio items or submenus, in the catalog's order. A hidden command is not
    // in it; a disabled one is, and cannot be triggered.
    MenuBar {
        Dock.edge: Tui.Top
        vimNavigation: true
        palette.window: Theme.menu.window
        palette.windowText: Theme.menu.windowText
        palette.highlight: Theme.menu.highlight
        palette.highlightedText: Theme.menu.highlightedText
        palette.accent: Theme.menu.accent
        Instantiator {
            model: App.menu
            Menu {
                title: model.label
                Instantiator {
                    model: model.rows
                    DelegateChooser {
                        role: "kind"
                        DelegateChoice {
                            roleValue: "item"
                            MenuItem { text: model.label; enabled: model.enabled; onTriggered: App.run(model.id) }
                        }
                        DelegateChoice {
                            roleValue: "radio"
                            MenuItem {
                                text: model.label; group: model.group; checked: model.checked
                                enabled: model.enabled; onTriggered: App.run(model.id)
                            }
                        }
                        DelegateChoice {
                            roleValue: "submenu"
                            Menu {
                                title: model.label
                                Instantiator {
                                    model: model.rows
                                    DelegateChooser {
                                        role: "kind"
                                        DelegateChoice {
                                            roleValue: "item"
                                            MenuItem { text: model.label; enabled: model.enabled; onTriggered: App.run(model.id) }
                                        }
                                        DelegateChoice {
                                            roleValue: "radio"
                                            MenuItem {
                                                text: model.label; group: model.group; checked: model.checked
                                                enabled: model.enabled; onTriggered: App.run(model.id)
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        // the dock, the bar's corner: the query, then each open panel; a click hides a shown panel
        // or shows a hidden one, and the query's hides or shows them all (tui/panels.go)
        Flex {
            direction: Tui.Horizontal
            Repeater {
                model: App.dock
                // a hidden panel's icon is dimmed
                Button { text: model.icon; flat: true; toolTipText: model.title; onClicked: App.goDock(model.key)
                         palette.buttonText: model.hidden ? Theme.document.lineNumber : Theme.menu.windowText }
            }
        }
    }

    // ---- the page: the query ------------------------------------------------
    //
    // The query editor is the page. The explorer and the results are golib's Panels over it
    // (below): each docked at its edge and taking its strip from the page (reserve), so the screen
    // reads as a split; Alt/Option-left drag floats one, Alt/Option-right drag resizes it, and the
    // dock in the menu bar's corner hides and shows them (tui/panels.go). The explorer's tree and
    // the query editor are declared by id, because the host works them: it opens a folder the
    // explorer's Enter names (explorerTree.toggleExpanded), and it reads the query and scaffolds one
    // into it (editor). The results are a view of host sources, a component.
    Frame {
        title: App.queryTitle
        palette.window: Theme.document.window
        palette.windowText: Theme.document.windowText
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        palette.base: Theme.document.base
        palette.text: Theme.document.text
        // The keyboard starts here, and every card gives it back here.
        Editor {
            id: editor
            focus: true
            keyset: App.keyset
            // Undo, Redo, Copy, Cut and Paste at a right click, in the
            // App's language (golib's stock rows).
            contextMenu: true
            onTextChanged: App.queryEdited()   // an open note is now unsaved
            // The SQL of the connection the query runs on; the host
            // follows the connection (syntax.go).
            SyntaxHighlighter { definition: App.querySyntax }
        }
    }

    // ---- the status line ---------------------------------------------------
    //
    // left: the backend; centre: who is signed in; right: the last message.
    StatusBar {
        Dock.edge: Tui.Bottom
        palette.window: Theme.status.window
        palette.windowText: Theme.status.windowText
        left: App.statusLeft
        center: App.statusCenter
        right: App.status
    }

    // ---- the panels: the explorer and the results (tui/panels.go) -----------
    //
    // Each is the account's geometry: its size and length at its edge, and where it floats, kept as
    // it is dragged (onResized, onPlaced). Escape inside one closes it, as a Drawer does; the
    // leader's focus keys, View › Zoom and the dock bring it back.
    Panel {
        id: explorerPanel
        heading: qsTrId("autodb.main.title")
        reserve: true
        maximizable: true   // View › Zoom › Explorer
        edge: App.explorerEdge
        size: App.explorerSize
        length: App.explorerLength
        minimumSize: App.explorerMinSize; minimumLength: App.explorerMinLength   // auth.PanelSpecs' bounds
        floating: App.explorerFloating
        floatX: App.explorerX; floatY: App.explorerY; floatWidth: App.explorerW; floatHeight: App.explorerH
        onPlaced: App.panelPlaced("explorer", x, y, width, height)
        onResized: App.panelResized("explorer", size, length)
        onOpened: App.panelOpened("explorer")
        onClosed: App.panelClosed("explorer")
        onChanged: App.panelChanged("explorer", operation, maximized)
        Flex {
            direction: Tui.Vertical
            palette.window: Theme.document.window
            palette.windowText: Theme.document.windowText
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            palette.base: Theme.document.base
            palette.text: Theme.document.text
            // Enter on a row: a folder opens, a table scaffolds its query, and
            // a connection — or anything under one — becomes the query's.
            TreeView {
                id: explorerTree
                Layout.fillHeight: true
                model: App.explorer
                textRole: "label"
                badgeRole: "badge"
                // A row carries its semantic kind; the active theme chooses
                // its color. Selection colors still take precedence.
                delegate: Text {
                    text: model.label
                    color: model.kind === "ws" ? Theme.syntax.keyword :
                           model.kind === "conn" ? Theme.syntax.function :
                           model.kind === "schema" ? Theme.syntax.dataType :
                           model.kind === "sec" ? Theme.syntax.controlFlow :
                           model.kind === "tbl" ? Theme.syntax.attribute :
                           model.kind === "col" ? Theme.syntax.constant :
                           model.kind === "fn" ? Theme.syntax.specialChar :
                           model.kind === "note" ? Theme.syntax.string : palette.text
                }
                onActivated: App.explorerActivated(index)
                onCurrentIndexChanged: App.explorerMoved(index)
            }
        }
    }
    Panel {
        id: resultsPanel
        heading: qsTrId("autodb.main.title2")
        reserve: true
        maximizable: true
        edge: App.resultsEdge
        size: App.resultsSize
        length: App.resultsLength
        minimumSize: App.resultsMinSize; minimumLength: App.resultsMinLength   // auth.PanelSpecs' bounds
        floating: App.resultsFloating
        floatX: App.resultsX; floatY: App.resultsY; floatWidth: App.resultsW; floatHeight: App.resultsH
        onPlaced: App.panelPlaced("results", x, y, width, height)
        onResized: App.panelResized("results", size, length)
        onOpened: App.panelOpened("results")
        onClosed: App.panelClosed("results")
        onChanged: App.panelChanged("results", operation, maximized)
        Flex {
            id: results
            direction: Tui.Vertical
            palette.window: Theme.document.window
            palette.windowText: Theme.document.windowText
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            palette.base: Theme.document.base
            palette.text: Theme.document.text
            Text { text: App.resultsSummary }
            Results { id: resultsBody; visible: App.resultsBodyShown; Layout.fillHeight: true }
            Editor { id: resultsJSON; readOnly: true; visible: App.resultsAsJSON
                     text: App.resultsJSON; Layout.fillHeight: true }
        }
    }

    // ---- overlays: opened by id from a handler or by the host ----------------
    Leader { id: leader }
    Hints { id: hints }
    Help { id: help }
    About { id: about }
    Inspect { id: inspect }
    Value { id: value }
    Pressure { id: pressure }
    ConnCard { id: card }
    ConfirmQuit { id: quit }
    Login { id: login }
    RemoteConnect { id: remoteConnect }
    KeyPassphrase { id: keyPassphrase }
    StartChoice { id: startChoice }
    RemoteManage { id: remoteManage }
    RemoteServerForm { id: remoteServerForm }
    SSHKeyForm { id: sshKeyForm }
    Bootstrap { id: bootstrap }
    NoteName { id: noteName }
    NoteOpen { id: noteOpen }
    UnsavedNote { id: unsaved }
    NoteConflict { id: conflict }
    ConnPicker { id: connPicker }
    Confirm { id: confirm }
    Connections { id: connections }
    ConnectionForm { id: connectionForm }
    Attach { id: attach }
    Tokens { id: tokens }
    TokenForm { id: tokenForm }
    Widening { id: widening }
    Search { id: search }
    Workspaces { id: workspaceManager }
    WorkspaceName { id: workspaceName }
    WorkspaceAttach { id: workspaceAttach }
    Profile { id: profile }
    Users { id: users }
    UserForm { id: userForm }
    Addresses { id: addresses }
    History { id: history }
    HistoryFilter { id: historyFilter }
    Audit { id: audit }
    AuditFilter { id: auditFilter }
    CACert { id: caCert }
    ConfirmRestart { id: restart }
    Keyslot { id: keyslot }
    KeyslotConfirm { id: keyslotConfirm }
}
