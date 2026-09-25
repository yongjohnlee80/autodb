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
//
// The blueprint (blueprint/main.qml) is the whole design; this screen takes up
// its parts as they are built.

import tui 1.0
import autodb 1.0                 // App: this program's state and commands
import autodb.theme.retro 1.0     // the Theme singleton — Options › Theme switches it
import autodb.views 1.0           // Leader, Hints, Help, About
import autodb.dialogs 1.0         // ConfirmQuit

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

    // ---- keys ------------------------------------------------------------
    //
    // A Shortcut sees only what the focused widget leaves, and none is live
    // while a dialog is open — the dialog has the keyboard.
    Shortcut { sequence: "Space";  onActivated: leader.open() }
    Shortcut { sequence: "q";      onActivated: App.run("app.quit") }
    Shortcut { sequence: "Ctrl+Q"; onActivated: App.run("app.quit") }
    Shortcut { sequence: "?";      onActivated: App.showHints() }

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
    }

    // ---- the workspace -----------------------------------------------------
    //
    // The panes arrive with the workspace (the blueprint's Explorer,
    // QueryEditor and Results); until then the program says where it stands.
    Frame {
        palette.window: Theme.document.window
        palette.windowText: Theme.document.windowText
        Text { text: App.status }
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

    // ---- overlays: opened by id from a handler or by the host ----------------
    Leader { id: leader }
    Hints { id: hints }
    Help { id: help }
    About { id: about }
    ConfirmQuit { id: quit }
}
