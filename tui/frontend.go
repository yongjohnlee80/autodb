package tui

// Frontend says what this frontend can do about the daemon's lifetime.
//
// It exists because the program offers an admin action that SHUTS THE DAEMON
// DOWN, and whether that is safe depends entirely on who is hosting the
// program. In a terminal it is: the daemon exits, the next call fails, and the
// session's spawn function starts a replacement. Under `autodb --web-ui` the
// spawn is nil by design, so the same keystroke would strand every web
// session with no way to bring the daemon back — including sessions belonging
// to other people.
//
// The zero value is the terminal.
type Frontend uint8

const (
	// FrontendTerminal may shut the daemon down; a spawn restores it.
	FrontendTerminal Frontend = iota
	// FrontendWeb must not: nothing in this process will start one.
	FrontendWeb
)
