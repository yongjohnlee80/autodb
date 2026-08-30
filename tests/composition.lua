-- ADR-0078 composition matrix — the cases that need MORE THAN ONE plugin.
--
-- The per-plugin suites cover their own halves: autodb's smoke §[18] covers
-- the standalone composition (auto-core + autodb) and every provider-failure
-- path, and auto-finder's run-all covers auto-core + auto-finder with autodb
-- ABSENT (the placeholder contract). Neither can cover the two cases here,
-- because each needs the other plugin on the runtimepath:
--
--   2. auto-core + auto-finder + autodb, no autovim
--      auto-finder hosts the real drawer, buffer identity is EXACTLY what it
--      was before the renderer moved, and no autodb panel opens.
--   4. a runtime host transition
--      never two live hosts, buffers, or subscription sets.
--
-- Run:
--   AUTO_CORE=<path> AUTO_FINDER=<path> \
--     nvim --headless -u NONE -l tests/composition.lua
--
-- Driven with `-l` (the family convention's default) rather than sourced as
-- init: nothing here renders a TUI grid, so the exception autodb's smoke.lua
-- documents does not apply.

local function need(var)
  local p = os.getenv(var)
  if not p or p == "" then
    print("MISSING PRECONDITION: $" .. var .. " must point at the plugin root")
    os.exit(1)
  end
  vim.opt.runtimepath:append(p)
  return p
end

need("AUTO_CORE")
need("AUTO_FINDER")
vim.opt.runtimepath:append(vim.fn.getcwd())

local pass, fail = 0, 0
local function ok(name, cond, detail)
  if cond then
    pass = pass + 1
    print("  PASS  " .. name)
  else
    fail = fail + 1
    print("  FAIL  " .. name .. (detail and ("  — " .. tostring(detail)) or ""))
  end
end

local drawer = require("autodb.views.drawer")
local hostreg = drawer._host_for_tests
local panel_mod = require("auto-core").ui.panel

print("\n[C2] auto-core + auto-finder + autodb (no autovim)")
do
  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()

  -- Both plugins set up, in the order a lazy.nvim user would get them.
  -- dbase is NOT in auto-finder's defaults (a workspace adds it with
  -- `slot add dbase`), so the composition has to ask for it explicitly —
  -- which is exactly why "auto-finder is installed" is the wrong test for
  -- whether it hosts the drawer.
  require("autodb").setup({})
  require("auto-finder").setup({ sections = { "config", "files", "dbase" } })
  local dbase = require("auto-finder.views.dbase")
  ok("C2: auto-finder's dbase facade loads with autodb present", dbase ~= nil)
  ok("C2: the facade reports autodb as available", dbase._available_for_tests() == true)

  -- REGISTRATION IS NOT CALLED HERE. auto-finder.setup() must have done
  -- it, because a user opens the drawer without visiting the section
  -- first. Calling dbase.register() by hand is what masked this in the
  -- first submission (lector impl-r0 MF1).
  ok("C2: setup registered the facade as a drawer host, unprompted",
    drawer._host_for_tests ~= nil and (function()
      -- the winner for an open must be auto-finder, not the self-host
      local seen
      drawer.open(function(o, v) seen = o and v.host or nil end)
      local owner = drawer.owner()
      return owner == "auto-finder" and seen == "auto-finder"
    end)(), tostring(drawer.owner()))

  -- auto-finder outranks autodb's self-host, so an open goes to it.
  local oo, ov
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C2: the drawer mounts on AUTO-FINDER, not autodb",
    oo == true and ov and ov.host == "auto-finder", vim.inspect(ov))

  -- Criterion 2's real gate: identity is byte-for-byte what auto-finder's
  -- own smoke asserts, because the renderer moving must be invisible to it.
  local view = drawer.mounted_view()
  ok("C2: a view is mounted", view ~= nil)
  local b = view and view:bufnr()
  ok("C2: the drawer has a live buffer", b ~= nil and vim.api.nvim_buf_is_valid(b))
  ok("C2: filetype is auto-finder", b and vim.bo[b].filetype == "auto-finder",
    b and vim.bo[b].filetype)
  ok("C2: b:auto_finder_view == 'dbase'", b and vim.b[b].auto_finder_view == "dbase",
    b and tostring(vim.b[b].auto_finder_view))
  ok("C2: the buffer is named auto-finder://dbase",
    b and vim.api.nvim_buf_get_name(b):find("auto-finder://dbase", 1, true) ~= nil,
    b and vim.api.nvim_buf_get_name(b))

  -- ...and NO autodb panel opened.
  ok("C2: no autodb panel exists", panel_mod.get("autodb") == nil)

  -- Subscriptions are the drawer's own either way.
  ok("C2: the drawer holds its own subscriptions", view and view._sub_count() > 0,
    view and tostring(view._sub_count()))

  -- Closing the section releases central ownership (the facade calls the
  -- release it was handed), so the next open is a fresh mount.
  dbase.on_close()
  ok("C2: the facade's on_close releases central ownership", drawer.owner() == nil)
  ok("C2: and the view was disposed",
    view and view:bufnr() == nil and view._sub_count() == 0)
end

print("\n[C4] runtime host transition")
do
  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
  require("autodb.panel").setup()

  -- Start self-hosted: auto-finder's dbase section is not registered yet
  -- (it is not in auto-finder's default sections — it is added per-workspace
  -- with `slot add dbase`, so "auto-finder is installed" is NOT the test).
  local oo, ov
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C4: with no dbase host registered, autodb self-hosts",
    oo == true and ov.host == "autodb", vim.inspect(ov))
  local self_view = drawer.mounted_view()
  local self_buf = self_view:bufnr()
  ok("C4: the self-hosted drawer has a buffer", self_buf ~= nil)

  -- Now auto-finder gains dbase, through the REAL slot path.
  require("auto-finder").setup({ sections = { "config", "files" } })
  ok("C4: without a dbase slot, auto-finder is not a host",
    drawer.owner() == nil or drawer.owner() == "autodb")
  require("auto-finder")._rebuild_section_registry({ "config", "files", "dbase" },
    { no_force_open = true })
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C4: the higher-priority host takes over", oo == true and ov.host == "auto-finder",
    vim.inspect(ov))

  -- The one-owner invariant: the loser is gone BEFORE the winner mounts.
  ok("C4: the losing view was disposed", self_view:bufnr() == nil)
  ok("C4: the losing view's subscriptions are gone", self_view._sub_count() == 0)
  ok("C4: exactly one owner", drawer.owner() == "auto-finder")
  local af_view = drawer.mounted_view()
  ok("C4: the winner has its own, different buffer",
    af_view ~= self_view and af_view:bufnr() ~= nil and af_view:bufnr() ~= self_buf)
  ok("C4: no autodb panel is left open", panel_mod.get("autodb") == nil
    or not panel_mod.get("autodb"):_is_open())

  -- ...and back again: dbase is removed from the workspace, through the
  -- REAL rebuild rather than a hand-rolled unregister_host.
  require("auto-finder")._rebuild_section_registry({ "config", "files" },
    { no_force_open = true })
  ok("C4: unregistering the OWNER tears it down", drawer.owner() == nil)
  ok("C4: and disposes its view", af_view:bufnr() == nil and af_view._sub_count() == 0)
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C4: the drawer falls back to the self-host", oo == true and ov.host == "autodb",
    vim.inspect(ov))
  ok("C4: which is a fresh view", drawer.mounted_view() ~= self_view
    and drawer.mounted_view():bufnr() ~= nil)

  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
end

print("\n[C5] an unrelated slot rebuild must not disturb a live drawer")
do
  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
  require("autodb.panel").setup()
  require("auto-finder").setup({ sections = { "config", "files", "dbase" } })

  local oo, ov
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C5: the drawer is mounted on auto-finder", oo == true and ov.host == "auto-finder")
  local view = drawer.mounted_view()
  local buf = view and view:bufnr()
  local subs = view and view._sub_count()
  ok("C5: it has a live buffer and subscriptions", buf ~= nil and subs and subs > 0)

  -- A rebuild that leaves dbase ALONE (adds an unrelated slot). Syncing
  -- unconditionally re-registered the provider, and autodb's same-id
  -- rule tears the mounted owner down -- so an unrelated slot change
  -- disposed and remounted a live drawer (lector impl-r1).
  require("auto-finder")._rebuild_section_registry({ "config", "files", "dbase", "repos" },
    { no_force_open = true })

  ok("C5: the owner is unchanged", drawer.owner() == "auto-finder", tostring(drawer.owner()))
  ok("C5: it is the SAME view instance", drawer.mounted_view() == view)
  ok("C5: its buffer is still valid and unchanged",
    view and view:bufnr() == buf and buf and vim.api.nvim_buf_is_valid(buf),
    tostring(view and view:bufnr()) .. " vs " .. tostring(buf))
  ok("C5: its subscriptions survived", view and view._sub_count() == subs,
    tostring(view and view._sub_count()) .. " vs " .. tostring(subs))

  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
end

print("\n[C6] late load — the safety net must leave state authoritative")
do
  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
  require("autodb.panel").setup()
  -- auto-finder came up with dbase configured; registration then happened
  -- through the section's OWN late-load safety net rather than through
  -- _sync_dbase_host. A local "am I registered" boolean went stale here,
  -- and the next slot mutation either tore down a live drawer or left a
  -- dead provider behind (lector impl-r2 MF1).
  require("auto-finder").setup({ sections = { "config", "files", "dbase" } })
  local dbase = require("auto-finder.views.dbase")
  hostreg._reset_for_tests()                    -- simulate "autodb arrived late"
  require("autodb.panel").setup()
  ok("C6: after a registry reset the facade is not registered",
    dbase.is_registered() == false)

  -- The safety net registers as a side effect of a direct focus.
  local panel = require("auto-core").ui.panel.get("auto-finder")
  local w = panel and panel.winid or vim.api.nvim_get_current_win()
  dbase.get_buffer(w)
  ok("C6: the safety net registered the provider", dbase.is_registered() == true)
  ok("C6: ...and auto-finder is the owner", drawer.owner() == "auto-finder",
    tostring(drawer.owner()))
  local view = drawer.mounted_view()
  local buf = view and view:bufnr()
  local subs = view and view._sub_count()

  -- Survivor: an unrelated slot add must not disturb it, even though the
  -- registration came from the safety net rather than from setup.
  require("auto-finder")._rebuild_section_registry({ "config", "files", "dbase", "repos" },
    { no_force_open = true })
  ok("C6: an unrelated slot add leaves the SAME view", drawer.mounted_view() == view
    and drawer.owner() == "auto-finder", tostring(drawer.owner()))
  ok("C6: ...its buffer and subscriptions intact",
    view and view:bufnr() == buf and view._sub_count() == subs,
    tostring(view and view:bufnr()) .. "/" .. tostring(view and view._sub_count()))

  -- Removal must actually withdraw, so the drawer falls back rather than
  -- failing against a section that no longer exists.
  require("auto-finder")._rebuild_section_registry({ "config", "files" },
    { no_force_open = true })
  ok("C6: removing dbase withdraws the provider", dbase.is_registered() == false)
  local oo, ov
  drawer.open(function(o, v) oo, ov = o, v end)
  ok("C6: and the next open FALLS BACK to autodb, not host_failed",
    oo == true and ov.host == "autodb", vim.inspect(ov))

  hostreg._reset_for_tests()
  require("autodb.panel")._reset_for_tests()
end

print(string.format("\n%d passed, %d failed", pass, fail))
if fail > 0 then
  print("COMPOSITION-COMPLETE FAIL")
  io.stdout:flush()
  os.exit(1)
end
print("COMPOSITION-COMPLETE OK")
io.stdout:flush()
os.exit(0)
