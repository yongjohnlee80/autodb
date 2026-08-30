---autodb.panel — autodb's own drawer surface (ADR-0078 §3.3 / §3.5).
---
---The fallback host. When something better is installed — auto-finder
---with a `dbase` view — that host registers at a higher priority and
---this one never mounts. When autodb is installed **alone**, on
---auto-core only, this is what puts the explorer on screen.
---
---It is an ordinary consumer of `auto-core.ui.panel` + `auto-core.ui.section`,
---like `auto-core.tasks.ui` and auto-agents: nothing here needs
---auto-finder.
---
---Two details are easy to get wrong and are handled deliberately:
---
---  1. **`panel.new` is idempotent per NAME, and merges opts into the
---     existing instance.** Reusing another plugin's name silently
---     replaces its callbacks, so this panel is named `autodb` and must
---     never be named `auto-finder`.
---  2. **`Panel:close()` does NOT fan section hooks out.** Only
---     `Registry:dispose()` does. So the panel's `on_close` disposes the
---     section registry (which also deletes cached buffers and
---     unregisters the winbar click router) and then calls the host
---     registry's `release`, without which the drawer would stay
---     "mounted" as far as autodb is concerned and the next open would
---     focus a dead surface (ADR-0078 §3.5, r2 MF1).
---
---@module 'autodb.panel'

local log = require("autodb.log")

local PANEL_NAME = "autodb"
local SECTION = 0

local M = {}

---@type any|nil  the auto-core panel instance
local panel = nil
---@type any|nil  auto-core's SECTION registry (not the drawer host registry)
local section_registry = nil
---@type fun()|nil  the release handed to us by the drawer host registry
local release_mount = nil
---@type autodb.DrawerView|nil
local current_view = nil

---teardown is the one idempotent close path. It is reached from the
---panel's own `on_close` (a real `q`, or `:close`), and from the host
---registry when this provider loses a handoff.
---@param notify_release boolean  false when the host registry already knows
local function teardown(notify_release)
  local reg, rel = section_registry, release_mount
  section_registry, release_mount, current_view = nil, nil, nil
  if reg then
    -- dispose() fans out section on_close, deletes cached buffers and
    -- unregisters the winbar click router — all of which a hand-rolled
    -- section loop would leave implicit.
    local ok, e = pcall(function() reg:dispose() end)
    if not ok then
      log.warn("panel", "section registry dispose failed: " .. tostring(e))
    end
  end
  if notify_release and rel then
    -- Tell the drawer host registry the surface is gone, so it disposes
    -- the view and drops its owner pointer.
    pcall(rel)
  end
end

---ensure_panel creates (or re-adopts) the autodb panel.
local function ensure_panel()
  if panel then return panel end
  local panel_mod = require("auto-core").ui.panel
  panel = panel_mod.new({
    name = PANEL_NAME,
    side = "left",
    width = { default = 40, min = 30, max = 100 },
    filetype = "autodb",
    on_close = function()
      -- The host registry has NOT been told yet: this is the
      -- host-initiated direction, so release() is ours to call.
      teardown(true)
    end,
  })
  return panel
end

---is_open reports whether our panel window is currently up.
local function is_open()
  return panel ~= nil and panel:_is_open()
end

-- ─── the host provider (ADR-0078 §3.3) ────────────────────────

---provider is registered at priority 0 — the reserved floor, so any
---real panel host outranks it.
M.provider = {
  id = "autodb",
  priority = 0,

  -- autodb can always host: auto-core is a hard dependency, so if we
  -- are running at all, a panel is available.
  available = function()
    local ok = pcall(require, "auto-core")
    return ok
  end,

  -- Identity is autodb's own; the drawer's DEFAULT_PROFILE already
  -- carries it, and editor_target_winid is the one host service.
  profile = {
    filetype      = "autodb",
    buf_var       = "autodb_view",
    buf_var_value = "drawer",
    buf_name      = "autodb://drawer",
    ---Pick a real editor window to open a note in: skip floats, skip
    ---anything pinned with winfixbuf, and skip ANY plugin's panel via
    ---auto-core's cross-plugin marker rather than auto-finder's private
    ---one — which is what lets this work without auto-finder installed.
    editor_target_winid = function()
      for _, w in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
        local cfg = vim.api.nvim_win_get_config(w)
        local floating = cfg and cfg.relative ~= nil and cfg.relative ~= ""
        local ok_fix, fixed = pcall(function() return vim.wo[w].winfixbuf end)
        local marker = vim.w[w].auto_core_panel_name
        local is_panel = type(marker) == "string" and marker ~= ""
        if not floating and not is_panel and not (ok_fix and fixed) then
          local b = vim.api.nvim_win_get_buf(w)
          if vim.bo[b].buftype == "" then return w end
        end
      end
      return nil
    end,
  },

  ---mount opens the panel, attaches a one-section registry over the
  ---view, and returns the window the drawer landed in so the host
  ---registry can validate it.
  ---@param view autodb.DrawerView
  ---@param release fun()
  ---@return integer? winid
  mount = function(view, release)
    local p = ensure_panel()
    local winid = p:open()
    if not winid then return nil end

    local section_mod = require("auto-core").ui.section
    section_registry = section_mod.attach(p, {
      {
        number = SECTION,
        name = "explorer",
        get_buffer = function(pn) return view:get_buffer(pn.winid) end,
        on_focus = function(pn, b) return view:on_focus(pn.winid, b) end,
        -- The view's teardown belongs to the host registry, not to a
        -- section hook: dispose() is called through release()/the
        -- registry so it happens exactly once.
        on_close = function() end,
      },
    }, { default = SECTION })

    release_mount = release
    current_view = view
    section_registry:focus(SECTION)
    return p.winid
  end,

  ---focus re-focuses the mounted drawer and returns its window.
  ---@return integer? winid
  focus = function()
    if not (panel and section_registry) then return nil end
    if not is_open() then
      local w = panel:open()
      if not w then return nil end
    end
    section_registry:focus(SECTION)
    return panel.winid
  end,

  ---close is OUR surface teardown, called by the host registry during a
  ---handoff. The registry disposes the view itself, so this must not
  ---call release() back at it.
  close = function()
    teardown(false)
    if panel and is_open() then
      pcall(function() panel:close() end)
    end
  end,
}

---setup registers autodb's own panel as the fallback drawer host.
---Cheap: it opens nothing and connects nothing.
function M.setup()
  -- The internal path: the public register_host refuses the reserved id
  -- outright, so a look-alike cannot displace this fallback (ADR-0078
  -- §3.3, lector impl-r1 MF2).
  local ok, err = require("autodb.views.host")._register_self(M.provider)
  if not ok then
    log.warn("panel", "could not register the autodb drawer host: " .. tostring(err and err.message))
  end
  return ok
end

---_reset_for_tests drops the panel and any section registry.
function M._reset_for_tests()
  teardown(false)
  if panel then
    pcall(function() panel:dispose() end)
  end
  panel = nil
end

M._is_open_for_tests = is_open

return M
