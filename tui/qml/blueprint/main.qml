// blueprint/main.qml — the whole screen, as it will be.
//
// It replaces main.qml once golib has what it uses (the blueprint test lists
// what is missing). STRUCTURE ONLY, like main.qml: no colour, no behaviour —
// every action is App.run(<command id>) through the catalog, or a named answer
// to a dialog.

import tui 1.0
import autodb 1.0
import autodb.theme.retro 1.0
import autodb.panels 1.0          // Results
import autodb.views 1.0           // Leader, Hints, Help, About, Pressure, Inspect, Value, ConnCard
import autodb.dialogs 1.0         // Login, Bootstrap, confirmations, note dialogs, pickers
import autodb.managers 1.0        // Connections, Users, Addresses, Tokens, Workspaces, History, Keyslot

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

    syntax.keyword: Theme.syntax.keyword
    syntax.controlFlow: Theme.syntax.controlFlow
    syntax.dataType: Theme.syntax.dataType
    syntax.string: Theme.syntax.string
    syntax.comment: Theme.syntax.comment
    syntax.decVal: Theme.syntax.decVal
    syntax.operator: Theme.syntax.operator

    // ---- keys ------------------------------------------------------------
    //
    // A Shortcut sees only what the focused widget leaves: the editor types
    // Space in Insert mode and leaves it in Normal mode.
    Shortcut { sequence: "Space";  onActivated: leader.open() }
    Shortcut { sequence: "Ctrl+Q"; onActivated: App.run("app.quit") }
    Shortcut { sequence: "q";      onActivated: quit.open() }
    Shortcut { sequence: "?";      onActivated: App.showHints() }
    Shortcut { sequence: "/";      onActivated: search.open() }
    Shortcut { sequence: "Ctrl+W"; onActivated: App.armZoom() }
    Shortcut { sequence: "Ctrl+H"; onActivated: App.run("focus.left") }
    Shortcut { sequence: "Ctrl+J"; onActivated: App.run("focus.down") }
    Shortcut { sequence: "Ctrl+K"; onActivated: App.run("focus.up") }
    Shortcut { sequence: "Ctrl+L"; onActivated: App.run("focus.right") }

    // ---- the menu bar: a view of the catalog -------------------------------
    //
    // App.menu is the catalog projected for who is signed in and what state
    // the program is in: categories → rows, each with its label, mnemonic,
    // enabled + reason, and command id. A hidden command is not in it.
    MenuBar {
        Dock.edge: Tui.Top
        vimNavigation: true
        palette.window: Theme.menu.window
        palette.windowText: Theme.menu.windowText
        palette.highlight: Theme.menu.highlight
        palette.highlightedText: Theme.menu.highlightedText
        palette.accent: Theme.menu.accent
        Repeater {
            model: App.menu
            Menu {
                title: model.label
                Repeater {
                    model: model.rows
                    MenuItem {
                        text: model.label
                        enabled: model.enabled
                        onTriggered: App.run(model.id)
                    }
                }
            }
        }
    }

    // ---- the workspace -----------------------------------------------------
    //
    // The schema on the left, the query above its results on the right. The
    // explorer's tree and the query editor are declared HERE, by id, because
    // the host works them: it opens a folder the explorer's Enter names
    // (explorerTree.toggleExpanded), and it reads the query and scaffolds one
    // into it (editor). The results are a view of host sources, a component.
    Split {
        orientation: Tui.Horizontal
        ratio: 0.25
        palette.window: Theme.document.window
        palette.windowText: Theme.document.windowText
        palette.highlight: Theme.document.highlight
        palette.base: Theme.document.base
        palette.text: Theme.document.text

        Frame {
            title: "explorer"
            // Enter on a row: a folder opens, a table scaffolds its query, and
            // a connection — or anything under one — becomes the query's.
            TreeView {
                id: explorerTree
                model: App.explorer
                textRole: "label"
                badgeRole: "badge"
                onActivated: App.explorerActivated(index)
            }
        }

        Split {
            orientation: Tui.Vertical
            ratio: 0.55
            Frame {
                title: App.queryTitle
                // The keyboard starts here, and every card gives it back here.
                Editor {
                    id: editor
                    focus: true
                    keyset: App.keyset
                }
            }
            Results { id: results }
        }
    }

    // ---- the status line -----------------------------------------------------
    //
    // left: editor mode and the backend; centre: who · where · which note;
    // right: the last message. A cleartext front door shows its banner left.
    StatusBar {
        Dock.edge: Tui.Bottom
        palette.window: Theme.status.window
        palette.windowText: Theme.status.windowText
        left: App.statusLeft
        center: App.statusCenter
        right: App.status
    }

    // ---- overlays: opened by id from a handler or by the host ----------------
    Leader { id: leader }
    Hints { id: hints }
    Help { id: help }
    About { id: about }
    Pressure { id: pressure }
    Inspect { id: inspect }
    Value { id: value }
    ConnCard { id: card }

    Login { id: login }
    Bootstrap { id: bootstrap }
    ConfirmQuit { id: quit }
    ConfirmRestart { id: restart }
    UnsavedNote { id: unsaved }
    NoteConflict { id: conflict }
    NoteName { id: noteName }
    ConnPicker { id: connPicker }
    Search { id: search }
    Profile { id: profile }

    Connections { id: connections }
    ConnectionForm { id: connectionForm }
    Users { id: users }
    UserForm { id: userForm }
    Addresses { id: addresses }
    Tokens { id: tokens }
    TokenForm { id: tokenForm }
    Widening { id: widening }
    Workspaces { id: workspaces }
    History { id: history }
    Keyslot { id: keyslot }
}
