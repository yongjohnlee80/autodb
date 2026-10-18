# AutoDB in AutoDoc

AutoDB runs inside [AutoDoc](https://github.com/yongjohnlee80/autodoc) as a plugin. It shows a
panel beside the page. The panel finds AutoDB (or installs the release that matches it), starts its
server, signs you in, and runs the statement under the cursor, the selection or the whole note.
The rows go to AutoDoc's own results table, a page at a time.

It needs AutoDoc with **plugin protocol 7**: the first AutoDoc release after v0.1.54. An older
AutoDoc refuses the manifest, so update AutoDoc first.

## Installing

**Plugins › Add from a git URL…** with this repository's URL. AutoDoc clones it, shows what it
will run, and asks first. Then it builds `cmd/autodoc-autodb` and starts it. The panel opens
on the right. **SPC p › AutoDB › o** shows or hides it again.

## The panel

Four tabs. **Alt+1** to **Alt+4** switch between them. **☰ Menu** holds the tab's actions. A row
that does not apply to the selection is greyed out, never hidden.

| Tab | What it shows | ☰ Menu |
|---|---|---|
| CONNECTIONS | the workspaces, their connections, and each connection's schemas, tables and columns | Add connection…, Test, Rename…, Delete…, New workspace…, Rename workspace…, Delete workspace…, Refresh |
| NOTES | a workspace's `.sql` notes | New note…, Open, Delete…, Refresh, Save to server, Save over the server's… |
| REMOTE | the remote AutoDB servers you have added | Add server…, Edit…, Connect, Disconnect, Delete… |
| SYSTEM | the plugin's and the server's versions, and who is signed in | Restart server…, Upgrade AutoDB…, Update the plugin…, Sign out, Retry |

**Enter** on a connection connects to it. Enter on a table opens a `SELECT` of it in a new buffer.

## Signing in once

The panel keeps your sign-in in AutoDoc's memory until AutoDoc quits. It is never written to disk.
Turning the plugin off and on, or updating it, does not ask for your passphrase again. Quitting
AutoDoc signs you out.

If the server restarted and its store is locked again, the panel asks for your passphrase with
your name filled in. A store with a service keyslot needs nothing.

## The first run

A new store has no user yet. The panel asks for the first one, its administrator: a name and a
passphrase of 8 characters or more, typed twice. With no workspace yet, it then offers to make
one, named **AutoDoc**.

## Connections

**Add connection…** asks for the database type, a name and the DSN. The DSN field is masked. For
TLS, name the CA, certificate and key files on the AutoDB server's host:

- **PostgreSQL**: they become `sslrootcert`, `sslcert` and `sslkey`. With a CA and no `sslmode`,
  `sslmode=verify-full` is added.
- **MySQL**: they become `tls-ca`, `tls-cert` and `tls-key`. AutoDB reads the files each time it
  connects, so a renewed certificate is picked up.
- **SQLite** has no TLS.

A certificate needs its key. The connection is made in the selected workspace, and is then tested.

## Notes

A note is a `.sql` file of a workspace.

- **The local server's notes** open as AutoDB's own files. AutoDoc's save, undo and reload from
  disk work on them as on any file.
- **A remote server's notes** open as a working copy in your cache
  (`$XDG_CACHE_HOME/autodoc-autodb/notes/`). A small `.autodb` file beside the copy records which
  server, user, workspace and note it belongs to. `:autodb save` (or **Save to server**) pushes the
  buffer to that note and to no other. A draft, another server's copy, or another user's copy is
  refused, and nothing is written.

If someone else changed the note since you fetched it, nothing is lost:

- **Your copy is unedited:** the panel asks whether to take the server's copy. **Yes** keeps
  yours as `<name>.~v<version>`.
- **Your copy is edited:** it is opened untouched. The server's text is written beside it as
  `<name>.server`. Compare the two, then **Save over the server's…**.

## Remote servers

**Add server…** asks for a name, the host and port of the server's SSH listener (7422 by
default), your AutoDB user there, and your SSH key. The key is either a file or held by the ssh
agent. The servers are AutoDB's own `remotes.toml`, shared with AutoDB's terminal UI.

- **The first Connect** from this machine asks for your passphrase twice: it enrolls this
  machine's device key. Then it shows the server's host key fingerprint. Check it against the
  one your administrator gave you. **Trust** pins it, and from then on a different key is
  refused.
- **While connected**, CONNECTIONS and NOTES are the remote server's. Restart and Upgrade are
  not offered: the server is its administrator's.
- **Disconnect** returns to the local server, still signed in.
- **Editing a server** ends its kept sign-in. Its old working copies are no longer that server's,
  and saving them is refused.

## Restarting and upgrading

- **Restart server…** restarts the local server only when nothing is running on it. If something
  is, the panel says what, and the server keeps running.
- **A server older than the plugin** (another protocol) opens SYSTEM, saying so. **Upgrade
  AutoDB…** installs the matching release into the plugin's `bin/`, signs in as an administrator,
  and restarts the server onto the new release once it is idle. If the new release does not
  start, the previous one is started again. A server too old to restart itself says to restart
  it by hand.
- **A plugin older than the server**: **Update the plugin…** asks AutoDoc to update it.

## `:autodb`

The plugin brings the command `:autodb`:

| Command | What it does |
|---|---|
| `:autodb` or `:autodb run` | runs the statement under the cursor, or the selection |
| `:autodb query <sql>` | runs `<sql>` |
| `:autodb script` | runs the whole note |
| `:autodb connect` | goes to the connection's choice |
| `:autodb cancel` | cancels the running query |
| `:autodb save` | saves the remote note to its server |
