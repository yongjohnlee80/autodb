// main.qml — autodb's screen, in QML.
//
// STRUCTURE ONLY: what is on the screen, where it is docked, and what each
// control triggers. It names no colour — the imported theme does — and it runs
// nothing itself: every action is App.run(<command id>), through the one
// catalog that decides what is offered to whom.
//
// A palette is set ONCE, where it starts: the Window carries the application
// palette; a surface that is a distinct part of the design overrides only its
// own roles; everything else inherits.

import tui 1.0
import autodb 1.0                 // App: this program's state and commands
import autodb.theme.retro 1.0     // the Theme singleton — Options › Theme switches it

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

    Shortcut { sequence: "Ctrl+Q"; onActivated: App.run("app.quit") }

    // ---- the workspace -----------------------------------------------------
    //
    // A placeholder until the panes are written: the foundation proves the
    // program assembles, binds and runs; the screens come next.
    Frame {
        palette.window: Theme.document.window
        palette.windowText: Theme.document.windowText
        Text { text: App.status }
    }

    // ---- the status line ---------------------------------------------------
    StatusBar {
        Dock.edge: Tui.Bottom
        palette.window: Theme.status.window
        palette.windowText: Theme.status.windowText
        left: App.backend
        center: App.identity
        right: App.auth
    }
}
