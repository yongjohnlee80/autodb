# The AutoDB panel for AutoDoc (protocol 7)

This AutoDB plugin update requires **AutoDoc with plugin protocol 7**, the first AutoDoc release
after v0.1.54. An older AutoDoc refuses the updated manifest, so update AutoDoc first.

The card is now a panel with four tabs ([the guide](../autodoc-plugin.md)):

- **CONNECTIONS:** workspaces and their connections as a tree. Add, test, rename and delete
  connections, and make, rename and delete workspaces. A connection's TLS files go into the DSN as
  the server reads them.
- **NOTES:** a workspace's notes. A local note opens as AutoDB's own file. A remote note opens as
  a working copy that `:autodb save` pushes back, and a conflict never loses an edit.
- **REMOTE:** AutoDB's remote servers. Connect through their SSH listener, the host key pinned on
  the first connect, and disconnect back to the local server.
- **SYSTEM:** versions, Restart when idle, Upgrade an older server onto the plugin's release,
  Update an older plugin through AutoDoc, and Sign out.

Also:

- **The sign-in is kept** in AutoDoc's memory: turning the plugin off and on or updating it does
  not ask again. Quitting AutoDoc signs out.
- **The first run** makes the first user and the first workspace.
- **`:autodb`** runs the plugin's commands from AutoDoc's command line, including
  `:autodb query <sql>`.

The AutoDB server changes with it:

- **MySQL connections take TLS files:** `tls-ca`, `tls-cert` and `tls-key` in the DSN, read at
  each connect.
- **`auth.whoami` answers `store_unlocked`.**
