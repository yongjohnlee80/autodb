# Neovim

## Install the plugin

The plugin talks to the same binary. Install it with your plugin manager —
here [lazy.nvim](https://github.com/folke/lazy.nvim):

```lua
{
  "yongjohnlee80/autodb",
  dependencies = {
    "yongjohnlee80/auto-core.nvim",       -- HARD: events, state, log, ui.*
    -- "yongjohnlee80/auto-finder.nvim",  -- OPTIONAL: hosts the drawer in
    --                                    -- its shared panel instead
  },
  opts = {},
}
```

**How the plugin finds the binary**, in order — it never guesses silently, and
a failure reports every path it tried:

1. `opts.bin`, if you set it — honoured or refused, never quietly replaced.
2. **`PATH`** — which covers mise, the [install script](../install.sh), a
   release archive, and any package manager. Nothing to configure.
3. A plugin-local `bin/autodb`, which is what a lazy.nvim `build` hook makes.
4. A managed cache under `stdpath("data")/autodb/bin/`.

If you would rather have the binary version-matched to the plugin checkout
than on your `PATH`, use a build hook instead of installing it separately:

```lua
{
  "yongjohnlee80/autodb",
  dependencies = { "yongjohnlee80/auto-core.nvim" },
  build = "make build",
  opts = {},
}
```

Then verify:

```vim
:checkhealth autodb
```

which reports the binary it resolved and from where, the endpoint, and the
connection and login state. `setup()` itself is deliberately cheap — it
connects nothing and opens nothing; the first command that needs the daemon
brings it up and prompts for login.

## Using it

autodb is a standalone plugin as well as a backend (see
[Install the plugin](#install-the-plugin) above).
**`auto-core.nvim` is a hard dependency** — every module goes through it for
events, state, logging and UI primitives, and there is no fallback. Everything
else is optional. `opts` takes `bin` / `config` / `auto_spawn` / `keys`.

### What you get standalone

Everything, including the explorer. With auto-core alone autodb **self-hosts
its own panel** for the drawer:

|                             |                                              |
| --------------------------- | -------------------------------------------- |
| `<leader>Dl`                | sign in — retry, or switch user              |
| `<leader>Dw`                | choose or create a workspace                 |
| `<leader>Dc`                | choose a connection                          |
| `<leader>Dn`                | choose or create a note                      |
| `<leader>Dr` / `<leader>DR` | run this SQL buffer / the visual selection   |
| `<leader>Dh`                | script history                               |
| `<leader>DX`                | maintenance — restart / refresh              |
| `:AutodbDrawer`             | toggle the database explorer drawer          |
| `:checkhealth autodb`       | binary, endpoint, connection and login state |

### What auto-finder adds

If [`auto-finder.nvim`](https://github.com/yongjohnlee80/auto-finder.nvim) is
installed and its `dbase` section is enabled, the **same** drawer renders in
auto-finder's shared panel instead of a second one — section switching with
`0..9`, one panel column, no duplication. autodb notices at open time and does
not self-host. Nothing needs configuring on either side.

The recommended setup is **autodb + auto-finder** (preferably under
[autovim](https://github.com/yongjohnlee80/autovim)). Standalone is a fully
supported configuration, not a degraded one.

### Driving it from Lua

Every `<leader>D` operation is a function on `require("autodb.api")`, so your
own keymaps get exactly the same surface — the API is the contract and the
built-in keymaps are one consumer of it.

```lua
local api = require("autodb.api")

vim.keymap.set("n", "<leader>qq", function() api.drawer_toggle() end)

-- Anything that talks to the daemon is async and reports through an
-- optional callback, so you can sequence it.
api.run_sql("select 1", function(ok, value)
  if not ok then
    -- value is { code, message, cause? }; `cancelled` means the user
    -- backed out, which is not the same as a failure.
    return vim.notify(value.message, vim.log.levels.ERROR)
  end
  -- value is { statements, result? }; result is nil for pure DDL and
  -- otherwise carries columns and the RAW row arrays.
  print(value.statements .. " statement(s)")
end)
```

`autodb.commands` and the other modules are internal and may change;
`autodb.api` is the supported surface. Host integration (`register_host`) lives
on `autodb.views.drawer`.
