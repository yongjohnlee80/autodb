-- :autodb for AutoDoc: the AutoDB plugin's commands from the command line. AutoDoc loads this file
-- (with package.toml) as the plugin's Lua package; Neovim never requires it.
autodoc.command{
  name = "autodb",
  args = "run | query <sql> | script | connect | cancel",
  complete = { "run", "query", "script", "connect", "cancel" },
  help = "AutoDB: run the statement under the cursor, a query, the whole page, pick a connection, cancel",
  run = function(ed, args)
    local ok, err = autodoc.plugin.run(args[1] or "run", select(2, unpack(args)))
    if not ok then error(err, 0) end
  end,
}
