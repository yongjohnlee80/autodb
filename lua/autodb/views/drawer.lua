---autodb.views.drawer — the autodb explorer drawer (ADR-0078, ADR-0058 §3.3).
---
---A pure renderer over autodb's Lua surface: every byte of data arrives
---through `autodb.session`, and this view never dials a daemon, holds a
---token, or re-derives which connection is active
---([[auto-family-state-ownership]], [[shared-resolver-single-source-of-truth]]).
---
---It lived in auto-finder until ADR-0078. It renders autodb's data, so
---autodb owns it: that is what lets autodb show a drawer when installed
---**alone**, and stops auto-finder from having to track autodb's session
---schema. auto-finder still displays it, now as a registered host (§3.3).
---
---**Instances, not a singleton.** `new(profile)` returns a view with its
---own buffer, rows, cache, expansion state and subscriptions. Two hosts
---must never share one buffer: `auto-core.ui.panel` restamps
---`b:auto_core_panel_owner` on every mount, so a shared buffer would
---flip owners and put the panel leak guard in conflict with the other
---window (ADR-0078 §3.2).
---
---**The profile is immutable and chosen before the buffer exists.**
---Buffer name, filetype and buffer var are the HOST's identity — it
---asserts on them and routes by them — while the highlight namespace,
---groups, help title and keymap descriptions are the drawer's own. The
---`AutoFinderDbase*` group names are kept verbatim on purpose: they are
---user-overridable, and renaming them would silently drop anyone's
---colorscheme overrides.
---
---Layout:
---
---  workspace
---    connection                  ← `*` marks the active one
---      Tables
---        schema.table
---
---`workspace.list` already embeds each workspace's connections, so the
---top two levels cost ONE request rather than 1+N. Notes are NOT here:
---they are client-side files under the configured notes directory, not
---an RPC surface, and belong with the commands that open them.
---
---Keymaps follow the panel vocabulary rather than the TUI's, because
---`h`/`l` are ordinary cursor motions in a scratch buffer here:
---
---  <CR>  toggle a container; on a NOTE file, open it in the editor
---        area so plain `:w` saves it
---  o     columns of the table under the cursor
---  i     info modal — connection details, table metadata
---  R     reload the node under the cursor
---  ?     help
---
---**Staleness.** Every async load is stamped with `session.epoch()` and
---dropped if the epoch moved while it was in flight: a reply describing
---a server we are no longer talking to must never paint (ADR-0058 MF3).
---Loads are also stamped per node, so a slow reply for a node the user
---has since collapsed is discarded rather than re-expanding it.
---@module 'autodb.views.drawer'

local logger = require("autodb.log")
local subs = require("autodb.views.subs")
local host = require("autodb.views.host")

---@class autodb.DrawerProfile
---@field filetype            string           -- buffer filetype
---@field buf_var             string           -- buffer-variable name
---@field buf_var_value       string           -- its value
---@field buf_name            string           -- full buffer name
---@field editor_target_winid fun(): integer?  -- host service: where to open a note

---DEFAULT_PROFILE is autodb's own identity, used by the self-host panel.
---@type autodb.DrawerProfile
local DEFAULT_PROFILE = {
  filetype      = "autodb",
  buf_var       = "autodb_view",
  buf_var_value = "drawer",
  buf_name      = "autodb://drawer",
  editor_target_winid = function() return nil end,
}

---@class autodb.DrawerView
---@field get_buffer fun(self, winid: integer): integer
---@field bufnr      fun(self): integer|nil
---@field on_focus   fun(self, winid: integer, bufnr: integer)
---@field dispose    fun(self)

-- ─── rendering identity (canonical autodb, NOT profile-derived) ───
--
-- The NAMESPACE is autodb's. The GROUP NAMES are deliberately the
-- historical AutoFinderDbase* ones: they are user-overridable, and
-- renaming them would silently drop anybody's colorscheme overrides
-- (lector r1 — highlight links do not reliably preserve override
-- precedence across colorscheme reload/order). Renaming them is its own
-- change, with its own deprecation.
--
-- These are module-level on purpose: they are constants shared by every
-- instance, and each instance still paints into its own buffer.

local NS = vim.api.nvim_create_namespace("autodb.drawer.hl")

local HL = {
  workspace  = "AutoFinderDbaseWorkspace",
  connection = "AutoFinderDbaseConnection",
  active     = "AutoFinderDbaseActive",
  group      = "AutoFinderDbaseGroup",
  item       = "AutoFinderDbaseItem",
  dim        = "AutoFinderDbaseDim",
  error      = "AutoFinderDbaseError",
}

local function _apply_default_highlights()
  local defs = {
    [HL.workspace]  = { link = "Title",      default = true },
    [HL.connection] = { link = "Directory",  default = true },
    [HL.active]     = { link = "Special",    default = true },
    [HL.group]      = { link = "Statement",  default = true },
    [HL.item]       = { link = "Normal",     default = true },
    [HL.dim]        = { link = "Comment",    default = true },
    [HL.error]      = { link = "ErrorMsg",   default = true },
  }
  for name, spec in pairs(defs) do
    pcall(vim.api.nvim_set_hl, 0, name, vim.deepcopy(spec))
  end
end

local M = {}

---bucket_relations selects the `want` kind out of a flat schema.tables result
---and nests Postgres partitions (ADR-0077) under their parent.
---
---Drawn flat, a parent with 50 partitions puts 51 siblings in the section and
---buries everything else. This returns only the TOP-LEVEL relations, each
---carrying its own partitions on `_partitions`.
---
---An orphan — a partition whose parent is not in this list, because it was
---filtered out or lives in another schema — stays top-level on purpose:
---nesting it under a folder that never gets drawn would make the relation
---disappear from the tree entirely.
---@param all table  raw schema.tables rows
---@param want string  the kind to keep ("table" / "view")
---@return table
---Keyed by SCHEMA then name, never by bare name. `schema.tables` is queried
---with an empty schema and so returns every schema at once: bare-name keys
---made same-named parents collide, and a partition of `schema_a.audit_log`
---attached itself to `schema_b.audit_log` as well — showing a relation under
---a parent it does not belong to. Nested maps are unambiguous by
---construction, where a flattened "schema.name" key would only move the
---ambiguity into the separator.
---
---`parent` is a SAME-SCHEMA relation name (ADR-0077), so presence is resolved
---within the partition's own schema rather than globally.
local function bucket_relations(all, want)
  all = type(all) == "table" and all or {}
  local present, byparent, out = {}, {}, {}
  local function sch(x) return x.schema or "" end

  for _, x in ipairs(all) do
    if (x.kind or "table") == want then
      local s = sch(x)
      present[s] = present[s] or {}
      present[s][x.name or ""] = true
    end
  end

  local function has_parent(x)
    local p = x.parent
    if not (x.is_partition and p and p ~= "") then return false end
    local s = sch(x)
    return present[s] ~= nil and present[s][p] == true
  end

  for _, x in ipairs(all) do
    if has_parent(x) then
      local s, p = sch(x), x.parent
      byparent[s] = byparent[s] or {}
      byparent[s][p] = byparent[s][p] or {}
      byparent[s][p][#byparent[s][p] + 1] = x
    end
  end

  for _, x in ipairs(all) do
    if (x.kind or "table") == want and not has_parent(x) then
      local s = sch(x)
      x._partitions = byparent[s] and byparent[s][x.name or ""] or nil
      out[#out + 1] = x
    end
  end
  return out
end

---new builds a drawer instance bound to one host profile.
---
---The profile is frozen here: buffer name, filetype, buffer vars,
---keymaps, highlight namespace and the panel-owner stamp are all
---established during mount and cannot be safely rewritten afterwards.
---@param profile autodb.DrawerProfile?
---@return autodb.DrawerView
function M.new(profile)
  profile = vim.tbl_extend("force", DEFAULT_PROFILE, profile or {})

  local st = {
    _bufnr = nil,
    _rows = nil,
    _subs = nil,
  }

  -- ─── autodb access (optional dependency) ──────────────────────

  ---_session returns autodb's session module, or nil when autodb is not
  ---installed. Nil is a normal state, not an error: the panel renders an
  ---explanation instead of failing to mount.
  local function _session()
    local ok, s = pcall(require, "autodb.session")
    if ok then return s end
    return nil
  end

  -- ─── tree state ───────────────────────────────────────────────

  ---@type table<string, boolean> node id -> expanded
  st._expanded = {}

  ---@type table<string, table> node id -> { loading, error, items, seq }
  st._cache = {}

  local _seq = 0
  local function _next_seq()
    _seq = _seq + 1
    return _seq
  end

  local function _cache(id)
    st._cache[id] = st._cache[id] or {}
    return st._cache[id]
  end

  ---_invalidate drops loaded children so the next expand refetches.
  ---@param id string?  nil clears everything
  ---_list_notes reads a workspace's note files. Notes are client-side
  ---files (never an RPC surface); autodb reports the root over its
  ---handshake, so both frontends and this panel read the same folder.
  function st._list_notes(ws_id)
    local ok, notes = pcall(require, "autodb.notes")
    if ok and notes and notes.list then return notes.list(ws_id) end
    return {}
  end

  function st.invalidate(id)
    if id then
      st._cache[id] = nil
    else
      st._cache = {}
    end
  end

  -- ─── loading ──────────────────────────────────────────────────

  local _rerender  -- forward declaration; defined after _render

  ---_load fetches a node's children exactly once per (node, epoch).
  ---
  ---Three guards, each for a failure this view has to survive:
  ---  * `entry.seq` — one in-flight request per node, so a repeated
  ---    expand does not stack duplicate loads.
  ---  * `session.guarded` — the epoch check; a reply from a previous
  ---    server never paints.
  ---  * `entry.seq ~= seq` on arrival — the node was collapsed or
  ---    invalidated while the reply was in flight, so the result is stale
  ---    even though the epoch still matches.
  local function _load(id, method, params, project)
    local s = _session()
    if not s then return end
    local entry = _cache(id)
    if entry.items or entry.loading then return end

    local seq = _next_seq()
    entry.loading, entry.seq, entry.error = true, seq, nil

    s.authed(method, params, s.guarded(function(result, err)
      local e = st._cache[id]
      if not e or e.seq ~= seq then return end   -- superseded
      e.loading = false
      if err then
        e.error = err.message or "error"
        e.items = nil
      else
        e.items = project and project(result) or result or {}
      end
      _rerender()
    end))
  end

  -- ─── row model ────────────────────────────────────────────────
  --
  -- Rows are built in one pass and kept in st._rows, parallel to the
  -- buffer lines, so the cursor position maps to a node without
  -- re-parsing text.

  local function _row(rows, lines, hls, opts)
    lines[#lines + 1] = opts.text
    rows[#rows + 1] = opts
    if opts.hl then
      hls[#hls + 1] = { lnum = #lines - 1, hl = opts.hl }
    end
    return #lines
  end

  local function _chevron(expanded)
    return expanded and "" or ""
  end

  ---_render paints the whole tree. Idempotent.
  local function _render(bufnr)
    if not (bufnr and vim.api.nvim_buf_is_valid(bufnr)) then return end
    _apply_default_highlights()

    local lines, rows, hls = {}, {}, {}
    local s = _session()

    if not s then
      _row(rows, lines, hls, { kind = "message", hl = HL.dim,
        text = "  autodb is not installed." })
      _row(rows, lines, hls, { kind = "message", hl = HL.dim,
        text = "  Install yongjohnlee80/autodb to use this panel." })
    elseif not s.is_ready() then
      _row(rows, lines, hls, { kind = "message", hl = HL.dim,
        text = "  Not connected." })
      _row(rows, lines, hls, { kind = "message", hl = HL.dim,
        text = "  <leader>Dw workspace · <leader>Dc connection · <leader>Dl sign in · ? help" })
    else
      local active_conn = s.connection()

      -- Workspaces are the root. They are loaded once per epoch.
      local root = _cache("root")
      if not root.items and not root.loading then
        -- workspace.list embeds each workspace's connections, so the top
        -- two levels of the tree cost ONE round trip rather than 1+N.
        _load("root", "workspace.list", {}, function(r)
          return type(r) == "table" and r or {}
        end)
      end

      if root.loading then
        _row(rows, lines, hls, { kind = "message", hl = HL.dim, text = "  loading…" })
      elseif root.error then
        _row(rows, lines, hls, { kind = "message", hl = HL.error,
          text = "  " .. tostring(root.error) })
      else
        -- One recursive-ish pass, no gotos. Containers ask _expanded
        -- whether to descend; every lazy level loads through _load and
        -- settles to loading / error / empty / items.
        local active_conn = active_conn
        local IND = "  "
        local function msg(depth, text, hl)
          _row(rows, lines, hls, { kind = "message", hl = hl or HL.dim,
            text = string.rep(IND, depth) .. text })
        end
        -- container renders a chevron row and returns whether it is open.
        local function container(depth, opts)
          local open = st._expanded[opts.id] == true
          _row(rows, lines, hls, {
            kind = opts.kind, id = opts.id, expandable = true,
            workspace = opts.workspace, connection = opts.connection,
            group = opts.group, item = opts.item, hl = opts.hl,
            text = string.rep(IND, depth) .. _chevron(open)
              .. " " .. (opts.marker or "") .. opts.label,
          })
          return open
        end
        -- children lazily loads id via method/params, then hands the
        -- settled items to draw (or renders loading/error/empty first).
        local function children(id, method, params, project, depth, empty, draw)
          local g = _cache(id)
          if not g.items and not g.loading then _load(id, method, params, project) end
          if g.loading then return msg(depth, "loading…") end
          if g.error then return msg(depth, tostring(g.error), HL.error) end
          if #(g.items or {}) == 0 then return msg(depth, empty) end
          draw(g.items)
        end

        -- table / view → its columns (schema.columns). Also what o toggles.
        local function draw_relation(depth, ws, conn, item, kind)
          local label = item.name or tostring(item)
          if item.schema and item.schema ~= "" then label = item.schema .. "." .. label end
          -- The node id is built BEFORE the badge: expansion state is keyed by
          -- it, so folding a relation must not depend on a cosmetic suffix.
          local id = string.format("%s:%d:%s", kind, conn.id, label)
          -- Bracketed so the role reads as an annotation rather than as part of
          -- the relation's name: "audit_log [partitioned]". Matches the TUI.
          if item.partitioned then label = label .. " [partitioned]" end
          local open = container(depth, {
            kind = kind, id = id, workspace = ws, connection = conn,
            item = item, hl = HL.item, label = label,
          })
          if not open then return end
          children(id .. ":cols", "schema.columns",
            { conn.id, item.schema or "", item.name },
            function(r) return type(r) == "table" and r or {} end,
            depth + 1, "(no columns)", function(cols)
              for _, c in ipairs(cols) do
                local badge = c.type or ""
                if c.pk then badge = badge .. " pk" end
                if c.nullable == false then badge = badge .. " not null" end
                _row(rows, lines, hls, { kind = "column", hl = HL.dim,
                  text = string.rep(IND, depth + 1) .. (c.name or "?")
                    .. (badge ~= "" and ("  " .. badge) or "") })
              end
            end)
          -- A partitioned parent also carries its partitions, bucketed by the
          -- section's projection. They are already in hand, so this draws them
          -- directly rather than issuing another schema.tables call — and
          -- recurses, so a partition's own columns work exactly as a table's.
          local parts = item._partitions
          if parts and #parts > 0 then
            local pid = id .. ":parts"
            if container(depth + 1, {
              kind = "group", id = pid, group = "partitions",
              workspace = ws, connection = conn, hl = HL.group,
              label = string.format("partitions (%d)", #parts),
            }) then
              for _, p in ipairs(parts) do
                draw_relation(depth + 2, ws, conn, p, kind)
              end
            end
          end
        end

        -- the tables / views / functions sections under a connection.
        local function draw_sections(depth, ws, conn)
          for _, sec in ipairs({
            { key = "tables", label = "tables", kind = "table" },
            { key = "views", label = "views", kind = "view" },
            { key = "functions", label = "functions" },
          }) do
            local sid = string.format("sec:%d:%s", conn.id, sec.key)
            local open = container(depth, {
              kind = "group", id = sid, group = sec.key,
              workspace = ws, connection = conn, hl = HL.group, label = sec.label,
            })
            if not open then goto next_sec end
            if sec.key == "functions" then
              children(sid, "schema.routines", { conn.id, "" },
                function(r) return type(r) == "table" and r.routines or {} end,
                depth + 1, "(none — engine has no stored routines)", function(items)
                  for _, r in ipairs(items) do
                    _row(rows, lines, hls, { kind = "routine", hl = HL.item,
                      text = string.rep(IND, depth + 1) .. (r.name or "?")
                        .. (r.signature and r.signature ~= "" and ("  " .. r.signature) or "") })
                  end
                end)
            else
              local want = sec.kind
              children(sid, "schema.tables", { conn.id, "" },
                function(r) return bucket_relations(r, want) end,
                depth + 1, "(none)", function(items)
                  for _, item in ipairs(items) do
                    draw_relation(depth + 1, ws, conn, item, sec.kind)
                  end
                end)
            end
            ::next_sec::
          end
        end

        for _, ws in ipairs(root.items or {}) do
          local ws_id = "ws:" .. tostring(ws.id)
          if not container(0, { kind = "workspace", id = ws_id, workspace = ws,
            hl = HL.workspace, label = ws.name or ws.id }) then goto next_ws end

          -- connections/
          local conns_open = container(1, { kind = "group", id = "conns:" .. tostring(ws.id),
            group = "connections", workspace = ws, hl = HL.group, label = "connections" })
          if conns_open then
            local ws_conns = ws.connections or {}   -- already in hand from workspace.list
            if #ws_conns == 0 then
              msg(2, "(no connections — <leader>Dc to add one)")
            else
              for _, conn in ipairs(ws_conns) do
                local is_active = active_conn and active_conn.id == conn.id
                local c_open = container(2, {
                  kind = "connection", id = "conn:" .. tostring(conn.id),
                  workspace = ws, connection = conn,
                  hl = is_active and HL.active or HL.connection,
                  marker = is_active and "* " or "", label = conn.name or conn.id,
                })
                if c_open then draw_sections(3, ws, conn) end
              end
            end
          end

          -- notes/
          local notes_open = container(1, { kind = "group", id = "notes:" .. tostring(ws.id),
            group = "notes", workspace = ws, hl = HL.group, label = "notes" })
          if notes_open then
            local nid = "notes:" .. tostring(ws.id)
            local g = _cache(nid)
            if not g.items and not g.loading then
              g.items = st._list_notes(ws.id)   -- client-side files; synchronous
            end
            if #(g.items or {}) == 0 then
              msg(2, "(no notes — a to create)")
            else
              for _, note in ipairs(g.items) do
                _row(rows, lines, hls, { kind = "note", hl = HL.item,
                  workspace = ws, item = note,
                  text = string.rep(IND, 2) .. (note.name or note.path) })
              end
            end
          end
          ::next_ws::
        end
      end
    end

    if #lines == 0 then
      _row(rows, lines, hls, { kind = "message", hl = HL.workspace,
        text = "  No workspaces yet." })
      _row(rows, lines, hls, { kind = "message", hl = HL.dim,
        text = "  <leader>Dw to create one · ? for help" })
    end

    -- Restore the cursor line across repaints so a background refresh
    -- does not move the user (the no-hijack invariant).
    local win = vim.fn.bufwinid(bufnr)
    local cursor = win ~= -1 and vim.api.nvim_win_get_cursor(win) or nil

    vim.bo[bufnr].modifiable = true
    vim.api.nvim_buf_clear_namespace(bufnr, NS, 0, -1)
    vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, lines)
    for _, h in ipairs(hls) do
      pcall(vim.api.nvim_buf_set_extmark, bufnr, NS, h.lnum, 0,
        { end_line = h.lnum + 1, hl_group = h.hl })
    end
    vim.bo[bufnr].modifiable = false

    if cursor and win ~= -1 then
      local lnum = math.min(cursor[1], math.max(#lines, 1))
      pcall(vim.api.nvim_win_set_cursor, win, { lnum, cursor[2] })
    end

    st._rows = rows
  end

  _rerender = function()
    if st._bufnr and vim.api.nvim_buf_is_valid(st._bufnr) then
      _render(st._bufnr)
    end
  end

  ---_row_under_cursor maps the cursor to its row entry.
  local function _row_under_cursor(panel_winid)
    if not (st._rows and panel_winid and vim.api.nvim_win_is_valid(panel_winid)) then
      return nil
    end
    local lnum = vim.api.nvim_win_get_cursor(panel_winid)[1]
    return st._rows[lnum]
  end

  -- ─── actions ──────────────────────────────────────────────────

  ---_toggle expands or collapses a container.
  ---
  ---Collapsing INVALIDATES the node, so re-expanding refetches rather
  ---than showing a tree that may have changed on the server since.
  local function _toggle(row)
    if not row or not row.id or not row.expandable then return false end
    if st._expanded[row.id] then
      st._expanded[row.id] = nil
      st.invalidate(row.id)
      st.invalidate(row.id .. ":cols")   -- table/view columns live here
    else
      st._expanded[row.id] = true
    end
    _rerender()
    return true
  end

  ---_open_note opens a note file in the EDITOR area, not the panel.
  ---
  ---Requirement 5: once it is a real buffer on a real path, plain `:w`
  ---saves it where autodb expects, with no bespoke save command.
  local function _open_note(row)
    local path = row and row.item and (row.item.path or row.item.file)
    if not path then return end
    local target = profile.editor_target_winid and profile.editor_target_winid() or nil
    if target then
      pcall(vim.api.nvim_set_current_win, target)
      pcall(vim.cmd, "edit " .. vim.fn.fnameescape(path))
    else
      -- No editor window: make one rather than replacing a panel.
      pcall(vim.cmd, "botright vsplit " .. vim.fn.fnameescape(path))
    end
  end

  ---_scaffold is `<CR>` on a table/view: write a SELECT note over the
  ---SERVER-quoted identifier and open it (ADR-0058). Not a query run — a
  ---starting point you edit and `:w`.
  local function _scaffold(row)
    if not (row and row.workspace and row.item) then return end
    local ok, notes = pcall(require, "autodb.notes")
    if not ok then return end
    local path, err = notes.scaffold(row.workspace.id, row.item.name or "query", row.item.quoted)
    if not path then
      logger.notify("autodb: " .. tostring(err), { level = vim.log.levels.ERROR })
      return
    end
    st._expanded["notes:" .. tostring(row.workspace.id)] = true
    _open_note({ item = { path = path } })
  end

  ---_add_note is `a`: create a note in the row's workspace.
  local function _add_note(row)
    local ws = row and row.workspace
    if not ws then
      logger.notify("autodb: put the cursor on a workspace's notes to add one",
        { level = vim.log.levels.WARN })
      return
    end
    local ok, notes = pcall(require, "autodb.notes")
    if not ok then return end
    vim.ui.input({ prompt = "autodb — new note name: " }, function(name)
      name = name and vim.trim(name) or ""
      if name == "" then return end
      local path, err = notes.create(ws.id, name, "")
      if not path then
        logger.notify("autodb: " .. tostring(err), { level = vim.log.levels.ERROR })
        return
      end
      st._expanded["notes:" .. tostring(ws.id)] = true
      _open_note({ item = { path = path } })
    end)
  end

  ---_delete_note is `d`: remove the note under the cursor, after a prompt.
  local function _delete_note(row)
    if not (row and row.kind == "note" and row.workspace and row.item) then return end
    local ok, notes = pcall(require, "autodb.notes")
    if not ok then return end
    local name = row.item.name or vim.fn.fnamemodify(row.item.path or "", ":t")
    if vim.fn.confirm("Delete note '" .. name .. "'?", "&Yes\n&No", 2) ~= 1 then return end
    local dok, derr = notes.delete(row.workspace.id, name)
    if not dok then
      logger.notify("autodb: " .. tostring(derr), { level = vim.log.levels.ERROR })
    end
  end

  ---_activate is `<CR>`: toggle a container, open/scaffold, select a
  ---connection.
  local function _activate(row)
    if not row then return end
    if row.kind == "note" then return _open_note(row) end
    if row.kind == "connection" then
      -- Entering a connection makes it the active one AND expands it: you
      -- came here to work with it.
      local s = _session()
      if s then
        s.select_workspace(row.workspace)
        s.select_connection(row.connection)
      end
      _toggle(row)
      return
    end
    -- <CR> on a table/view scaffolds a SELECT note; columns are `o`.
    if row.kind == "table" or row.kind == "view" then return _scaffold(row) end
    -- Containers (workspace, groups) toggle; leaves (columns, routines)
    -- have nothing to open — `i` describes them.
    _toggle(row)
  end

  ---_columns is `o`: the columns of the table under the cursor.
  local function _columns(row)
    -- Columns hang off a table/view as its children; o reveals them, the
    -- same expansion <CR> toggles. A no-op elsewhere.
    if not row or (row.kind ~= "table" and row.kind ~= "view") then return end
    _toggle(row)
  end

  ---_info is `i`: what this node actually is, in a float.
  local function _info(row)
    if not row then return end
    local lines
    if row.kind == "connection" then
      local c = row.connection
      lines = {
        "Connection: " .. tostring(c.name or c.id),
        "  id:        " .. tostring(c.id),
        "  driver:    " .. tostring(c.driver or c.type or "?"),
        "  workspace: " .. tostring(row.workspace and row.workspace.name or "?"),
        "",
        "The DSN is held server-side, encrypted under the master key,",
        "and is never sent to the frontend.",
      }
    elseif row.kind == "table" then
      lines = {
        "Table: " .. tostring(row.item.schema and (row.item.schema .. ".") or "")
          .. tostring(row.item.name),
        "  connection: " .. tostring(row.connection.name or row.connection.id),
        "",
        "o — columns",
      }
    elseif row.kind == "note" then
      lines = {
        "Note: " .. tostring(row.item.name or row.item.path),
        "  path: " .. tostring(row.item.path or "?"),
        "",
        "<CR> opens it in the editor; :w saves it in place.",
      }
    else
      return
    end

    local ok, float = pcall(require, "auto-core.ui.float")
    if ok and float and float.help_overlay then
      pcall(float.help_overlay, lines, { title = "dbase" })
    else
      logger.notify(table.concat(lines, "\n"), { level = vim.log.levels.INFO })
    end
  end

  ---_reload drops the node's children and repaints.
  local function _reload(row)
    if row and row.id then
      st.invalidate(row.id)
    else
      st.invalidate(nil)
    end
    _rerender()
  end

  local HELP = {
    "autodb drawer — explorer",
    "",
    "  workspace → connections · notes",
    "  connection → tables · views · functions → columns",
    "",
    "  <CR>  expand · open a note · scaffold a SELECT on a table · enter a conn",
    "  o     expand a table/view into its columns",
    "  a     new note in this workspace     d  delete the note under the cursor",
    "  i     info about the node            R  reload (all with no node)",
    "  ?     this help",
    "",
    "  <leader>Dw  choose / create a workspace",
    "  <leader>Dc  choose / create a connection",
    "  <leader>Dr  run the buffer            <leader>DR  run the selection",
    "  <leader>Dh  history",
    "  <leader>Dl  sign in (retry / switch)",
    "  <leader>Dn  choose / create a note",
    "",
    "  Result grid (after running a query):",
    "  J     toggle table ⇄ JSON layout    <CR>  inspect the cell (full value)",
    "  y     yank cell   Y  yank row (CSV)  gy    yank row (JSON)",
    "  <Tab> / <S-Tab>  next / prev cell",
  }

  local function _help()
    local ok, float = pcall(require, "auto-core.ui.float")
    if ok and float and float.help_overlay then
      -- help_overlay(lines, opts) — lines is positional. Passing one packed
      -- {title, lines} table made `lines` a hash with no array part, so the
      -- overlay rendered "(no help entries)" and `?` looked dead.
      pcall(float.help_overlay, HELP, { title = "dbase" })
    else
      logger.notify(table.concat(HELP, "\n"), { level = vim.log.levels.INFO })
    end
  end

  -- ─── keymaps and subscriptions ────────────────────────────────

  local function _apply_keymaps(bufnr, panel_winid)
    if not vim.api.nvim_buf_is_valid(bufnr) then return end
    local set = function(lhs, fn, desc)
      pcall(vim.keymap.set, "n", lhs, fn, {
        buffer = bufnr, silent = true, nowait = true, desc = desc,
      })
    end
    set("<CR>", function() _activate(_row_under_cursor(panel_winid)) end,
      "autodb drawer: toggle / open note in the editor / select connection")
    set("o", function() _columns(_row_under_cursor(panel_winid)) end,
      "autodb drawer: columns of the table under the cursor")
    set("i", function() _info(_row_under_cursor(panel_winid)) end,
      "autodb drawer: info about the node under the cursor")
    set("R", function() _reload(_row_under_cursor(panel_winid)) end,
      "autodb drawer: reload the node under the cursor")
    set("a", function() _add_note(_row_under_cursor(panel_winid)) end,
      "autodb drawer: add a note to this workspace")
    set("d", function() _delete_note(_row_under_cursor(panel_winid)) end,
      "autodb drawer: delete the note under the cursor")
    set("?", _help, "autodb drawer: help")
  end

  ---_ensure_subscriptions keeps exactly one handler per topic.
  ---
  ---`view_subs:replace` rather than a `_subscribed` boolean: the boolean
  ---form silently survives a bus reset and the view then stops updating
  ---with no sign anything is wrong ([[view-subs-over-subscribe-flags]]).
  local function _ensure_subscriptions()
    st._subs = st._subs or subs.new()

    local s = _session()
    if not s then return end

    -- autodb publishes to auto-core topics, so these go through the same
    -- view_subs machinery as every other subscription in this plugin:
    -- one handle per slot, replaced rather than accumulated on refocus,
    -- and released together in on_close.
    st._subs:replace("autodb-connected", s.TOPIC_CONNECTED, function()
      st.invalidate(nil)
      vim.schedule(_rerender)
    end)
    st._subs:replace("autodb-disconnected", s.TOPIC_DISCONNECTED, function()
      st.invalidate(nil)
      vim.schedule(_rerender)
    end)
    st._subs:replace("autodb-selection", s.TOPIC_SELECTION, function()
      vim.schedule(_rerender)
    end)
    if s.TOPIC_WORKSPACES then
      st._subs:replace("autodb-workspaces", s.TOPIC_WORKSPACES, function()
        st.invalidate("root")
        vim.schedule(_rerender)
      end)
    end
    if s.TOPIC_NOTES then
      st._subs:replace("autodb-notes", s.TOPIC_NOTES, function(payload)
        local ws = type(payload) == "table" and payload.workspace or nil
        st.invalidate(ws and ("notes:" .. tostring(ws)) or nil)
        vim.schedule(_rerender)
      end)
    end
  end

  local function _dispose_subscriptions()
    if st._subs and st._subs.dispose_all then pcall(function() st._subs:dispose_all() end) end
    st._subs = nil
  end

  -- ─── public — the DrawerView contract (ADR-0078 §3.2) ─────────

  local view = {}

  ---get_buffer builds the buffer on first call and reuses it after.
  ---A host calls this during `mount` to obtain the buffer it puts in
  ---its window; auto-core's section registry reaches it the same way
  ---through the self-host provider's section definition.
  function view:get_buffer(winid)
    if st._bufnr and vim.api.nvim_buf_is_valid(st._bufnr) then
      _apply_keymaps(st._bufnr, winid)
      _ensure_subscriptions()
      return st._bufnr
    end
    local b = vim.api.nvim_create_buf(false, true)
    vim.bo[b].bufhidden = "hide"
    vim.bo[b].buftype   = "nofile"
    vim.bo[b].swapfile  = false
    vim.bo[b].filetype  = profile.filetype
    vim.b[b][profile.buf_var] = profile.buf_var_value
    pcall(vim.api.nvim_buf_set_name, b, profile.buf_name)
    _render(b)
    _apply_keymaps(b, winid)
    st._bufnr = b
    _ensure_subscriptions()
    return b
  end

  ---bufnr is what the host registry validates a mount against: nil
  ---before the first get_buffer, the live buffer while mounted, and nil
  ---again after dispose. After an accepted mount it equals the buffer
  ---displayed by the validated winid (ADR-0078 §3.2/§3.3).
  function view:bufnr()
    if st._bufnr and vim.api.nvim_buf_is_valid(st._bufnr) then return st._bufnr end
    return nil
  end

  function view:on_focus(winid, bufnr)
    if not vim.api.nvim_buf_is_valid(bufnr) then return end
    _render(bufnr)
    _apply_keymaps(bufnr, winid)
    _ensure_subscriptions()
  end

  ---dispose is the ONLY teardown, and is idempotent: buffer and
  ---subscriptions go together. Called by the host registry, or via the
  ---`release` handed to a provider's mount — never by a provider.
  function view:dispose()
    _dispose_subscriptions()
    if st._bufnr and vim.api.nvim_buf_is_valid(st._bufnr) then
      pcall(vim.api.nvim_buf_delete, st._bufnr, { force = true })
    end
    st._bufnr = nil
    st._rows = nil
    st._expanded = {}
    st._cache = {}
  end

  -- Test-only handles onto this instance's internals.
  view._st = st
  view._profile = profile
  view._sub_count = function() return st._subs and st._subs:count() or 0 end
  view._row_under_cursor = _row_under_cursor
  view._render_for_tests = _render
  view._activate_for_tests = _activate
  view._toggle_for_tests = _toggle
  view._invalidate = function(id) st.invalidate(id) end

  return view
end

-- ─── host registry re-exports (ADR-0078 §3.3) ─────────────────
--
-- Host integration is canonically addressed on THIS module;
-- `autodb.api` is reserved for end-user operations (lector r3).

M.register_host = host.register_host
M.unregister_host = host.unregister_host
M.has_host = host.has_host
M.open = host.open
M.focus = host.focus
M.toggle = host.toggle
M.owner = host.owner
M.mounted_view = host.view

M.DEFAULT_PROFILE = DEFAULT_PROFILE
M._HL = HL
M._NS = NS
M._host_for_tests = host
M._bucket_relations_for_tests = bucket_relations

return M
