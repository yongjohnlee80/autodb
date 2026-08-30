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
local _prompting = false

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

  -- A client whose socket is still good but which never got a token
  -- needs a LOGIN, not a second daemon. This is the path back from a
  -- mistyped passphrase, and it must not resolve a binary, probe the
  -- endpoint, or open a second connection to get there.
  local live = session.client()
  if live and live:is_ready() then
    return M._login(live, function(lok, lerr) _settle(lok, lerr) end)
  end

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

---_secret reads a passphrase without rendering it.
---
---`vim.ui.input` is the wrong tool for a secret: it echoes every
---keystroke, and its `highlight` field cannot conceal them — a custom
---handler paints the raw text into a float just as faithfully as the
---builtin paints it onto the cmdline. `inputsecret()` is the only core
---prompt that masks (it renders `*`), so a passphrase deliberately does
---NOT go through the configured UI, while a username — not a secret —
---does.
---
---One function because it is the only place this plugin ever reads a
---secret; a second call site is how the echo would quietly come back
---([[shared-resolver-single-source-of-truth]]).
---@param prompt string
---@return string|nil  nil when the user cancelled
function M._secret(prompt)
  -- <C-c> raises rather than returning, and an error escaping here would
  -- strand `_connecting` with every later command queued behind it.
  local got, val = pcall(vim.fn.inputsecret, prompt)
  vim.cmd("redraw")   -- take the prompt line back off the screen
  if not got or type(val) ~= "string" or val == "" then return nil end
  return val
end

---_login authenticates, bootstrapping the first admin if the store is
---empty (requirement 19).
---
---Prompting is suppressed without a UI, during a headless run, or when
---`vim.g.autodb_noninteractive` is set: a plugin that blocks a scripted
---Neovim on a passphrase prompt is a plugin that breaks CI.
---
---`opts.force` is how a deliberate press differs from an automatic
---connect. Only ONE prompt may be open at a time, but the guard cannot
---be allowed to strand the plugin: a third-party `vim.ui.input` handler
---that drops its callback on cancel would otherwise hold the flag for
---the rest of the session and refuse every later attempt — the same
---one-slip wedge this change exists to remove. So the automatic path
---defers to an open prompt, and an explicit key press replaces it.
---@param c AutodbClient
---@param cb fun(ok: boolean, err: string|nil)
---@param opts { force: boolean? }?
function M._login(c, cb, opts)
  if #vim.api.nvim_list_uis() == 0 or vim.g.autodb_noninteractive then
    return cb(false, "autodb: not connected (no UI for a login prompt)")
  end
  if _prompting and not (opts and opts.force) then
    return cb(false, "autodb: a login prompt is already open")
  end
  _prompting = true
  local function settle(ok, err)
    _prompting = false
    return cb(ok, err)
  end
  c:call("auth.needs_bootstrap", {}, function(needs, err)
    if err then return settle(false, err.message) end
    local first = needs == true
    vim.ui.input({ prompt = first and "autodb — create admin user: " or "autodb — user: " },
      function(name)
        if not name or name == "" then return settle(false, "autodb: login cancelled") end
        -- The name prompt can settle from inside its UI handler's own
        -- window teardown, so the masked prompt is deferred rather than
        -- opened on top of a closing float.
        vim.schedule(function()
          local pass = M._secret("autodb — passphrase: ")
          if not pass then return settle(false, "autodb: login cancelled") end
          local method = first and "auth.bootstrap" or "auth.login"
          c:call(method, { name, pass }, function(res, lerr)
            if lerr then return settle(false, lerr.message) end
            -- bootstrap and login both hand back a session token.
            local token = type(res) == "table" and (res.token or res[1]) or res
            if type(token) ~= "string" then
              return settle(false, "autodb: login returned no token")
            end
            c._token = token
            -- Remember who: autodb.api.login's documented success value
            -- is { user = ... }, and the name is only known here.
            M._signed_in_user = name
            log.notify("signed in as " .. name, { component = "auth" })
            settle(true, nil)
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
  -- Register autodb's own panel as the FALLBACK drawer host (ADR-0078
  -- §3.3), at the reserved priority 0 so any real panel host outranks
  -- it. This only registers: it opens no window and connects nothing,
  -- so `setup()` stays cheap. Resolution happens when the user opens
  -- the drawer, never here — load order between plugins is not
  -- guaranteed, so deciding a host at setup time would be a coin flip.
  pcall(function() require("autodb.panel").setup() end)
  return M
end

---signed_in_user is the name of the last successful sign-in, or nil.
---@return string|nil
function M.signed_in_user()
  return M._signed_in_user
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

  local c = session.client()
  local connected, signed_in, conn_label = false, false, "none selected"
  if c and c:is_ready() then
    connected = true
    h.ok("connected · instance " .. tostring(c:instance()))
    -- Connected and signed in are different states, and this is the
    -- surface where a user works out which one they are stuck in.
    if c:token() then
      signed_in = true
      h.ok("signed in")
    else
      h.warn("connected but NOT signed in — press " .. keys.LOGIN .. " to sign in")
    end
    local conn = session.connection()
    conn_label = conn and (conn.name or conn.id) or "none selected"
    h.info("connection: " .. tostring(conn_label))
    local status, message = lifecycle.build_status(c:hello().version, bin)
    if status == "stale" then h.warn(message) else h.info(message) end
  else
    h.info("not connected (a " .. keys.PREFIX .. " command will connect)")
  end

  -- ...and RETURN the same facts as data. `:checkhealth` renders through
  -- vim.health, but autodb.api.health() is documented to hand a caller a
  -- table (ADR-0078 §3.6) — it returned nil, which is no contract at all.
  return {
    binary = bin,
    binary_error = (not bin) and (berr or "no autodb binary found") or nil,
    connected = connected,
    signed_in = signed_in,
    user = M._signed_in_user,
    connection = conn_label,
  }
end

return M
