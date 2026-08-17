---autodb.nvim — a database IDE inside Neovim, over autodb's own daemon.
---
---`setup()` is deliberately cheap and CONNECTS NOTHING. Starting a
---daemon and prompting for a passphrase because the user opened Neovim
---would be a poor trade for a plugin they may not touch this session.
---The first command that needs the backend brings it up.
---@module 'autodb'

local commands = require("autodb.commands")
local keys = require("autodb.keys")
local lifecycle = require("autodb.lifecycle")
local log = require("autodb.log")
local session = require("autodb.session")

local M = {}

---@class AutodbConfig
---@field bin string?           explicit binary path; otherwise discovered
---@field config string?        autodb config file passed to the binary
---@field auto_spawn boolean?   start a daemon when none is listening (default true)
---@field keys table?           per-command overrides; `false` disables one
M.config = {
  bin = nil,
  config = nil,
  auto_spawn = true,
  keys = {},
}

local _connecting = false
local _waiters = {}

local function _settle(ok, err)
  _connecting = false
  local waiters = _waiters
  _waiters = {}
  for _, cb in ipairs(waiters) do pcall(cb, ok, err) end
end

---ensure_connected brings the backend up and logs in, once.
---
---Concurrent callers QUEUE rather than each starting their own attempt:
---two commands pressed in quick succession must not spawn two daemons
---or open two passphrase prompts.
---@param cb fun(ok: boolean, err: string|nil)
function M.ensure_connected(cb)
  if session.is_ready() then return cb(true, nil) end
  _waiters[#_waiters + 1] = cb
  if _connecting then return end
  _connecting = true

  local bin, berr = lifecycle.resolve_binary(M.config.bin)
  if not bin then return _settle(false, berr) end

  local ep, eerr = lifecycle.resolve_endpoint(bin, M.config.config)
  if not ep then return _settle(false, eerr) end

  local function connect()
    local client = require("autodb.client")
    client.connect({
      addr = ep.addr,
      mode = ep.mode,
      on_lost = function(reason)
        session.detach(reason)
      end,
    }, function(c, cerr)
      if not c then return _settle(false, cerr) end
      session.attach(c, { bin = bin })
      M._login(c, function(lok, lerr) _settle(lok, lerr) end)
    end)
  end

  if lifecycle.is_listening(ep) then return connect() end
  if not M.config.auto_spawn then
    return _settle(false, lifecycle.describe_manual(
      "nothing is listening on " .. ep.addr .. " and auto_spawn is off"))
  end
  lifecycle.spawn({ bin = bin, config_path = M.config.config, endpoint = ep },
    function(sok, serr)
      if not sok then return _settle(false, serr) end
      connect()
    end)
end

---_login authenticates, bootstrapping the first admin if the store is
---empty (requirement 19).
---
---Prompting is suppressed without a UI, during a headless run, or when
---`vim.g.autodb_noninteractive` is set: a plugin that blocks a scripted
---Neovim on a passphrase prompt is a plugin that breaks CI.
function M._login(c, cb)
  if #vim.api.nvim_list_uis() == 0 or vim.g.autodb_noninteractive then
    return cb(false, "autodb: not connected (no UI for a login prompt)")
  end
  c:call("auth.needs_bootstrap", {}, function(needs, err)
    if err then return cb(false, err.message) end
    local first = needs == true
    vim.ui.input({ prompt = first and "autodb — create admin user: " or "autodb — user: " },
      function(name)
        if not name or name == "" then return cb(false, "autodb: login cancelled") end
        vim.ui.input({ prompt = "autodb — passphrase: ", highlight = function() return {} end },
          function(pass)
            if not pass or pass == "" then return cb(false, "autodb: login cancelled") end
            local method = first and "auth.bootstrap" or "auth.login"
            c:call(method, { name, pass }, function(res, lerr)
              if lerr then return cb(false, lerr.message) end
              -- bootstrap and login both hand back a session token.
              local token = type(res) == "table" and (res.token or res[1]) or res
              if type(token) ~= "string" then
                return cb(false, "autodb: login returned no token")
              end
              c._token = token
              log.notify("signed in as " .. name, { component = "auth" })
              cb(true, nil)
            end)
          end)
      end)
  end)
end

---setup wires the commands. Safe to call more than once.
---@param opts AutodbConfig?
function M.setup(opts)
  M.config = vim.tbl_deep_extend("force", M.config, opts or {})
  commands.setup({
    ensure_connected = M.ensure_connected,
    keys = M.config.keys,
  })
  return M
end

---health backs `:checkhealth autodb`.
function M.health()
  local h = vim.health or require("health")
  h.start("autodb")

  local bin, berr, label = lifecycle.resolve_binary(M.config.bin)
  if bin then
    h.ok(string.format("binary: %s (%s)", bin, label))
    local v = lifecycle.binary_version(bin)
    if v then h.info("version: " .. v) end
  else
    h.error(berr or "no autodb binary found")
  end

  if bin then
    local ep = lifecycle.resolve_endpoint(bin, M.config.config)
    if ep then
      h.info(string.format("endpoint: %s (%s)", ep.addr,
        ep.mode == "pipe" and "unix socket" or "tcp"))
      if lifecycle.is_listening(ep) then
        h.ok("a daemon is listening")
      else
        h.warn("nothing is listening; it will start on first use")
      end
    end
  end

  if session.is_ready() then
    local c = session.client()
    h.ok("connected · instance " .. tostring(c:instance()))
    local conn = session.connection()
    h.info("connection: " .. (conn and (conn.name or conn.id) or "none selected"))
    local status, message = lifecycle.build_status(c:hello().version, bin)
    if status == "stale" then h.warn(message) else h.info(message) end
  else
    h.info("not connected (a " .. keys.PREFIX .. " command will connect)")
  end
end

return M
