// Package tui implements autodb's standalone terminal UI on golib/tui,
// following the ddex-server blueprint: root model built first (its log pane
// is the logger sink), Dock/Split/Tabs/Table layout, overlay floats, async
// work via task results, and all data access through the rpc client seam -
// never through core directly (ADR-0052 §4).
//
// Keybindings follow neovim/autovim conventions. Implementation lands at
// roadmap milestone M6.
package tui
