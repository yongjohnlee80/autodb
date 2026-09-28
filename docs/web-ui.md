# The browser frontend

```sh
autodb --serve                    # start the backend first (it does not auto-start here)
autodb --web-ui --port=7010       # then serve the TUI to a browser
```

`--web-ui` serves the **same** TUI you get from `--ui`, in a browser, over a
WebSocket, talking to an already-running `--serve` daemon over RPC exactly as
`--ui` does. It is off unless you ask for it.

**It never starts the backend, and it fails fast if none is running.** Unlike
`--ui` — which spawns a daemon when it cannot find one — `--web-ui` exits with
an error naming the address. A browser frontend that silently started a
database daemon would be a surprise in the wrong direction.

**Access it over SSH, not a public bind.** `--web-ui` binds `127.0.0.1` only,
and `golib/tui/web` refuses a non-loopback bind without TLS:

```sh
ssh -L 7010:127.0.0.1:7010 your-host      # then open http://127.0.0.1:7010/
```

**First login on a fresh backend creates the admin**, the same way `--ui`'s
first run does. The window is safe because there is nothing to protect during
setup: no connection can exist until a user does.

A few behaviours differ from the terminal:

- **One backend connection per user.** Three tabs share one login and one
  daemon connection. Closing a tab detaches it; your login survives until the
  last tab has been gone for the idle timeout (five minutes). Closing the last
  tab is not an immediate sign-out.
- **A reconnect resumes; a reload restarts.** A dropped network or a closed lid
  reconnects to the same session with workspace and history intact. A browser
  *reload* starts a fresh session.
- **Notes are personal, keyed by (user, workspace)** — `<notes>/u-<username>/ws-<id>/`,
  in both frontends. You see your own notes and nobody else's, and the same
  account sees the same notes in a terminal and in a browser. Notes resolve
  *after* you sign in: before that there is no identity, so there is
  deliberately no shared tree to fall back on. `SPC A` shows the exact root in
  use.
- **Notes written before per-user keying appear as `legacy notes (ws-N) —
  deprecated`.** They carry no owner, so nothing can decide whose they are.
  `Enter` reads one, `m` migrates it into your own notes (copy, read back,
  verify, then remove the original — refusing if the name collides), `d`
  deletes it. The tree is read-and-delete only, so it can only shrink.
- **Some Ctrl chords belong to the browser.** `Ctrl-L`, `Ctrl-W` and `Ctrl-T`
  never reach autodb and a page cannot take them back. Measured: `Ctrl-H`,
  `Ctrl-J`, `Ctrl-K` and every `Alt` chord do arrive — so pane motion is also
  bound to **`Alt-h/j/k/l`**. Use those in a browser.
- **No `SPC X`.** Nothing in the web process can start a daemon back up, and
  restarting it would strand every other browser session. Restart from a
  terminal.
- **The daemon's audit shows the gateway's address.** Every browser user's RPC
  calls reach the daemon from `127.0.0.1` (the web process), so the daemon's IP
  allowlist and audit log attribute the *address* to the gateway. The **user**
  is still recorded correctly. This is inherent to one process serving several
  people.

Browser text-machine behaviour — key handling, composition, paste, wide
characters — is owned and tested by `golib/tui/web` across Chromium, Firefox
and WebKit; autodb does not re-test it.
