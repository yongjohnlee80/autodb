---autodb's semantic RPC client — policy, identity and session.
---
---`auto-core.rpc` is deliberately transport-only: it will connect to
---any address and reports the wire's own errors verbatim. Everything
---that makes a connection *autodb's* lives here (ADR-0058 §3.2.1):
---
---  * **where we may connect** — loopback only, until the M9 transport
---    exists. This is an application rule, not a family one: a future
---    consumer of `auto-core.rpc` with an authenticated transport must
---    not inherit our M9 gate.
---  * **who may receive a credential** — the launch nonce, so a stale
---    daemon from another build or a foreign port occupant cannot
---    collect a passphrase by accident.
---  * **what an error means** — the transport hands up the raw msgpack
---    error slot; autodb projects `{code, message}` out of it.
---  * **session and epoch** — the token, and the `instance` that makes a
---    reconnect detectable as a NEW server process.
---
---Nothing here blocks the editor. `hello` is awaited before any other
---request is admitted, but it is awaited asynchronously.
---@module 'autodb.client'

local rpc = require("auto-core.rpc")

local M = {}

-- ADR-0056: the wire contract this client speaks. A mismatch is fatal
-- and reported with direction — an older server needs a refresh, a
-- newer one needs a newer plugin — because "protocol mismatch" alone
-- sends people to the wrong fix.
M.PROTOCOL = 4

---@class AutodbClientOpts
---@field addr string                 -- host:port, loopback only
---@field expect_nonce string?        -- SHA-256 digest from the spawn record
---@field trust_external boolean?     -- user has explicitly trusted an unproved daemon
---@field limits table?               -- auto-core.rpc frame limits
---@field on_lost fun(reason: string)? -- epoch ended

---@class AutodbClient
local Client = {}
Client.__index = Client

--- Loopback check. Refusing before we dial matters: a non-loopback
--- address must never receive so much as a hello, because the M9
--- transport that would protect it does not exist yet.
---@param addr string
---@return boolean ok, string? err
function M.is_loopback(addr)
  local host = tostring(addr):match("^%[?([^%]]*)%]?:%d+$")
  if not host or host == "" then
    return false, string.format("autodb: malformed address %q (want host:port)", addr)
  end
  if host == "localhost" or host == "::1" or host == "0:0:0:0:0:0:0:1" then
    return true
  end
  local a, b, c, d = host:match("^(%d+)%.(%d+)%.(%d+)%.(%d+)$")
  if a then
    a, b, c, d = tonumber(a), tonumber(b), tonumber(c), tonumber(d)
    if a > 255 or b > 255 or c > 255 or d > 255 then
      return false, string.format("autodb: malformed address %q", addr)
    end
    if a == 127 then return true end
  end
  return false, string.format(
    "autodb: refusing to connect to %s — M7 supports loopback only. " ..
    "Remote servers arrive with the M9 transport (TLS and rate limits); " ..
    "until then a non-loopback endpoint would receive credentials over " ..
    "an unauthenticated connection.", addr)
end

---project turns a RAW msgpack error slot into autodb's `{code, message}`.
---
---The transport refuses to know this shape, correctly — the wire allows
---any value there. Two forms actually occur: the server's own
---`{code = …, message = …}` map, and Neovim's `[type, message]` array
---from a peer that is not autodb at all.
---@param err any
---@return { code: integer|nil, message: string }
function M.project_error(err)
  if type(err) == "table" then
    if err.message ~= nil or err.code ~= nil then
      return { code = tonumber(err.code), message = tostring(err.message or "error") }
    end
    if type(err[2]) == "string" then
      return { code = tonumber(err[1]), message = err[2] }
    end
  end
  if type(err) == "string" then
    return { code = nil, message = err }
  end
  return { code = nil, message = vim.inspect(err) }
end

---connect dials, handshakes, and calls back with a ready client.
---
---Asynchronous by construction: `cb(client, err)` fires once. The
---handshake is enforced here rather than left to callers — no request
---is admitted before hello has been checked, so a protocol mismatch or
---an unproved daemon cannot be discovered halfway through a login.
---@param opts AutodbClientOpts
---@param cb fun(client: AutodbClient|nil, err: string|nil)
function M.connect(opts, cb)
  opts = opts or {}

  local loopback_ok, loopback_err = M.is_loopback(opts.addr or "")
  if not loopback_ok then
    return cb(nil, loopback_err)
  end

  local self = setmetatable({}, Client)
  self._addr = opts.addr
  self._expect_nonce = opts.expect_nonce
  self._trust_external = opts.trust_external and true or false
  self._on_lost = opts.on_lost
  self._token = nil
  self._ready = false

  local conn, cerr = rpc.connect({
    addr = opts.addr,
    limits = opts.limits,
    on_epoch_lost = function(reason)
      self._ready = false
      self._token = nil
      if self._on_lost then self._on_lost(reason) end
    end,
  })
  if not conn then return cb(nil, cerr) end
  self._conn = conn

  -- Hello first, always. Nothing else is admitted until it lands.
  conn:request("sys.hello", { { protocol = M.PROTOCOL, name = "autodb.nvim" } }, {},
    function(outcome)
      if outcome.status ~= "ok" then
        conn:close()
        if outcome.status == "error" then
          local p = M.project_error(outcome.error)
          return cb(nil, "autodb: handshake refused: " .. p.message)
        end
        return cb(nil, "autodb: handshake failed (" .. outcome.status .. ")")
      end

      local hello = outcome.value
      if type(hello) ~= "table" or hello.server ~= "autodb" then
        conn:close()
        return cb(nil, string.format(
          "autodb: %s is not an autodb server (hello: %s)", self._addr, vim.inspect(hello)))
      end

      local ok, err = self:_check_protocol(hello)
      if not ok then
        conn:close()
        return cb(nil, err)
      end

      ok, err = self:_check_identity(hello)
      if not ok then
        conn:close()
        return cb(nil, err)
      end

      self._hello = hello
      self._instance = hello.instance
      self._ready = true
      cb(self, nil)
    end)
end

---_check_protocol reports a mismatch with its DIRECTION.
---
---"Protocol mismatch" alone sends people to the wrong fix half the
---time; which side is behind determines whether to refresh the binary
---or update the plugin.
function Client:_check_protocol(hello)
  local got = tonumber(hello.protocol)
  if got == M.PROTOCOL then return true end
  if got and got < M.PROTOCOL then
    return false, string.format(
      "autodb: the server speaks protocol %d, this plugin speaks %d — " ..
      "the BINARY is older. Refresh it with <leader>DX → refresh autodb.", got, M.PROTOCOL)
  end
  return false, string.format(
    "autodb: the server speaks protocol %s, this plugin speaks %d — " ..
    "the PLUGIN is older. Update autodb.nvim.", tostring(got), M.PROTOCOL)
end

---_check_identity decides whether this endpoint may receive credentials.
---
---Managed mode: the daemon must return the nonce we generated for the
---generation we launched. Anything else — a missing nonce, a different
---one — is not our child, whatever pid it claims, and is refused
---outright rather than downgraded to a trust prompt.
---
---External mode: no nonce exists, so the decision is the user's. It has
---to have been made ALREADY (`trust_external`), because the point is to
---ask before credentials flow, not after.
function Client:_check_identity(hello)
  local presented = hello.launch_nonce

  if self._expect_nonce then
    if type(presented) ~= "string" or presented == "" then
      return false, string.format(
        "autodb: the daemon on %s presented no launch proof, but this " ..
        "plugin started one for this generation. Refusing to send " ..
        "credentials to a process it did not launch.", self._addr)
    end
    if vim.fn.sha256(presented) ~= self._expect_nonce then
      return false, string.format(
        "autodb: the daemon on %s presented the WRONG launch proof — it " ..
        "is not the process this plugin started. Refusing to send " ..
        "credentials.", self._addr)
    end
    self._managed = true
    return true
  end

  if not self._trust_external then
    return false, string.format(
      "autodb: %s is served by a daemon this plugin did not start. " ..
      "Confirm you trust it (pid %s, %s) before logging in.",
      self._addr, tostring(hello.pid), tostring(hello.addr))
  end
  self._managed = false
  return true
end

---call issues a request and projects the outcome into autodb's shape.
---
---`cb(result, err)` where `err` is `{code, message}` — never a raw wire
---value, so no caller has to know msgpack error shapes.
---@param method string
---@param params table
---@param cb fun(result: any|nil, err: table|nil)
---@param opts { deadline: integer? }?
function Client:call(method, params, cb, opts)
  if not self._ready or not self._conn or self._conn:is_closed() then
    return cb(nil, { code = nil, message = "autodb: not connected" })
  end
  local id, err = self._conn:request(method, params or {}, opts or {}, function(outcome)
    if outcome.status == "ok" then return cb(outcome.value, nil) end
    if outcome.status == "error" then return cb(nil, M.project_error(outcome.error)) end
    -- A client-side outcome: report the reason, not a server code that
    -- does not exist.
    cb(nil, { code = nil, message = "autodb: " .. outcome.status, client = outcome.code })
  end)
  if not id then
    cb(nil, { code = nil, message = "autodb: request refused (" .. tostring(err) .. ")",
      client = err })
  end
  return id
end

---login exchanges a passphrase for a session token, which is then
---attached to every authenticated call.
---@param name string
---@param passphrase string
---@param cb fun(ok: boolean, err: table|nil)
function Client:login(name, passphrase, cb)
  self:call("auth.login", { name, passphrase }, function(result, err)
    if err then return cb(false, err) end
    self._token = type(result) == "table" and result.token or result
    cb(self._token ~= nil, self._token and nil
      or { code = nil, message = "autodb: login returned no token" })
  end)
end

---authed is `call` with the session token as the first argument — the
---shape every authenticated autodb verb takes.
function Client:authed(method, params, cb, opts)
  if not self._token then
    return cb(nil, { code = nil, message = "autodb: not logged in" })
  end
  local args = { self._token }
  for _, v in ipairs(params or {}) do args[#args + 1] = v end
  return self:call(method, args, cb, opts)
end

function Client:token() return self._token end
function Client:instance() return self._instance end
function Client:hello() return self._hello end
function Client:addr() return self._addr end
function Client:is_managed() return self._managed == true end
function Client:is_ready() return self._ready and self._conn and not self._conn:is_closed() end
function Client:in_flight() return self._conn and self._conn:in_flight() or 0 end

---close ends the epoch. Idempotent.
function Client:close()
  self._ready = false
  self._token = nil
  if self._conn then self._conn:close() end
end

return M
