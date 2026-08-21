---autodb.notes — the workspace notes store, client-side.
---
---Notes are plain `.sql` files under `<notes_dir>/ws-<id>/`, NOT an RPC
---surface: both frontends (the TUI and this plugin) read and write the
---same folder, and `:w` in a real buffer is the save path. The daemon is
---the authority on the root (`session.notes_dir()`), and this module is
---the ONE place the layout, the name rules, and the mutations live so the
---tree view and the `<leader>Dn` command never diverge
---([[shared-resolver-single-source-of-truth]]). The naming rules mirror
---the server's `CleanName` exactly (single component, `.sql` suffix), so
---a name valid here is valid there.
---@module 'autodb.notes'

local session = require("autodb.session")
local log = require("autodb.log")

local M = {}

-- Mirror of the server's noteName: first char alnum, then alnum/._ -/space.
local NAME = "^[%w][%w._ -]*$"

---dir is the immutable-id-keyed workspace folder.
---@param ws_id integer
---@return string
function M.dir(ws_id)
  return session.notes_dir() .. "/ws-" .. tostring(ws_id)
end

---clean_name validates a display name and appends the `.sql` suffix,
---rejecting separators, dot-paths, and a leading dot — the same contract
---the server enforces.
---@param name string
---@return string|nil clean, string|nil err
function M.clean_name(name)
  name = (name or ""):gsub("%.sql$", "")
  if not name:match(NAME) then
    return nil, "invalid note name (single component, no separators, no leading dot)"
  end
  return name .. ".sql", nil
end

---path resolves a note's absolute path (validated).
---@param ws_id integer
---@param name string
---@return string|nil path, string|nil err
function M.path(ws_id, name)
  local clean, err = M.clean_name(name)
  if not clean then return nil, err end
  return M.dir(ws_id) .. "/" .. clean, nil
end

---list returns `{ name, path }` for every `.sql` file in the workspace,
---sorted by name. A missing folder is empty, not an error.
---@param ws_id integer
---@return { name: string, path: string }[]
function M.list(ws_id)
  local root = session.notes_dir()
  if type(root) ~= "string" or root == "" then return {} end
  local dir = M.dir(ws_id)
  local fs = vim.uv or vim.loop
  local handle = fs.fs_scandir(dir)
  if not handle then return {} end
  local out = {}
  while true do
    local name, typ = fs.fs_scandir_next(handle)
    if not name then break end
    if typ ~= "directory" and name:match("%.sql$") then
      out[#out + 1] = { name = name, path = dir .. "/" .. name }
    end
  end
  table.sort(out, function(a, b) return a.name < b.name end)
  return out
end

---create writes a new note (refusing to clobber an existing one) and
---announces the change so the explorer refreshes. `body` defaults to
---empty.
---@param ws_id integer
---@param name string
---@param body string?
---@return string|nil path, string|nil err
function M.create(ws_id, name, body)
  local path, err = M.path(ws_id, name)
  if not path then return nil, err end
  vim.fn.mkdir(M.dir(ws_id), "p", "0700")
  local fs = vim.uv or vim.loop
  if fs.fs_stat(path) then
    return nil, "a note named " .. vim.fn.fnamemodify(path, ":t") .. " already exists"
  end
  local ok, werr = pcall(function()
    local fd = assert(io.open(path, "w"))
    fd:write(body or "")
    fd:close()
  end)
  if not ok then return nil, "could not write note: " .. tostring(werr) end
  session.notes_changed(ws_id)
  return path, nil
end

---delete removes a note and announces the change.
---@param ws_id integer
---@param name string
---@return boolean ok, string|nil err
function M.delete(ws_id, name)
  local path, err = M.path(ws_id, name)
  if not path then return false, err end
  local fs = vim.uv or vim.loop
  local rok, rerr = (fs.fs_unlink and fs.fs_unlink(path)) or os.remove(path)
  if not rok then return false, "could not delete note: " .. tostring(rerr) end
  session.notes_changed(ws_id)
  return true, nil
end

---scaffold_sql builds a quick SELECT over the SERVER-quoted identifier —
---never a raw name, so a table with an exotic name can never break the
---generated SQL (the quoted form is a trusted value from schema.tables).
---@param quoted string
---@return string
function M.scaffold_sql(quoted)
  return "SELECT *\nFROM " .. quoted .. "\nLIMIT 100;\n"
end

---scaffold_name derives a valid, non-colliding note name from a base
---(usually a table name), so scaffolding `songs` twice yields `songs`
---then `songs-2` rather than clobbering.
---@param ws_id integer
---@param base string
---@return string
function M.scaffold_name(ws_id, base)
  base = tostring(base or "query"):gsub("[^%w._ -]", "_"):gsub("^[.]+", "")
  if base == "" then base = "query" end
  local fs = vim.uv or vim.loop
  local candidate, n = base, 1
  while fs.fs_stat(M.dir(ws_id) .. "/" .. candidate .. ".sql") do
    n = n + 1
    candidate = base .. "-" .. n
  end
  return candidate
end

---scaffold writes a SELECT note for a relation and returns its path.
---@param ws_id integer
---@param table_name string   display base for the file name
---@param quoted string       server-quoted identifier for the FROM clause
---@return string|nil path, string|nil err
function M.scaffold(ws_id, table_name, quoted)
  if type(quoted) ~= "string" or quoted == "" then
    return nil, "no server-quoted identifier for this relation"
  end
  local name = M.scaffold_name(ws_id, table_name)
  local path, err = M.create(ws_id, name, M.scaffold_sql(quoted))
  if not path then return nil, err end
  log.debug("notes", "scaffolded " .. path)
  return path, nil
end

return M
