-- autodb.nvim — smoke test driver
--
-- Run headless from the repo root:
--   nvim --headless -u tests/smoke.lua -c 'qa!'
--
-- Per the family convention, this driver is extended every iteration and
-- must run green before work is reported complete.
--
-- The end-to-end sections drive a REAL `autodb --serve` process over a
-- real socket. That is deliberate: the launch nonce, the handshake and
-- the identity refusal are the security boundary of this plugin, and a
-- mock server would agree with the client by construction.

local plugin_root = vim.fn.fnamemodify(
  vim.fn.fnamemodify(debug.getinfo(1, "S").source:sub(2), ":p"), ":h:h")
vim.opt.rtp:prepend(plugin_root)

-- auto-core is a hard dependency. Probe the sibling worktree first (the
-- development layout) and fall back to the installed plugin.
local core_paths = {
  vim.fn.fnamemodify(plugin_root, ":h:h") .. "/auto-core.nvim/main",
  vim.fn.stdpath("data") .. "/lazy/auto-core.nvim",
  vim.fn.expand("~/.local/share/nvim/lazy/auto-core.nvim"),
}
local core_found = false
for _, p in ipairs(core_paths) do
  if vim.fn.isdirectory(p .. "/lua/auto-core") == 1 then
    vim.opt.rtp:prepend(p)
    core_found = true
    break
  end
end
if not core_found then
  print("FATAL: auto-core.nvim not found in any of:\n  " .. table.concat(core_paths, "\n  "))
  vim.cmd("cq!")
end

local pass_count, fail_count = 0, 0
local function ok(name, cond, detail)
  if cond then
    pass_count = pass_count + 1
    print("  PASS  " .. name)
  else
    fail_count = fail_count + 1
    print("  FAIL  " .. name .. (detail and ("  — " .. tostring(detail)) or ""))
  end
end
local function eq(a, b) return vim.deep_equal(a, b) end

print("autodb.nvim smoke")

-- ─────────────────── [1] client — address policy ───────────────────
print("\n[1] client.is_loopback — where we may connect (ADR-0058 §3.2.1)")
;(function()
  local client = require("autodb.client")

  for _, addr in ipairs({ "127.0.0.1:7419", "127.5.6.7:1", "localhost:7419", "[::1]:7419" }) do
    ok("p1: accepts loopback " .. addr, (client.is_loopback(addr)) == true)
  end

  -- Refused BEFORE dialing: a non-loopback endpoint must never receive
  -- so much as a hello while the M9 transport does not exist.
  for _, addr in ipairs({ "10.0.0.5:7419", "192.168.1.10:7419", "example.com:7419",
    "0.0.0.0:7419", "[2001:db8::1]:7419" }) do
    local okl, err = client.is_loopback(addr)
    ok("p1: refuses non-loopback " .. addr, okl == false and err ~= nil, tostring(err))
  end

  local _, m9err = client.is_loopback("10.0.0.5:7419")
  ok("p1: the refusal names M9 so the reader knows what would fix it",
    tostring(m9err):find("M9", 1, true) ~= nil, m9err)

  for _, bad in ipairs({ "", "127.0.0.1", "nonsense", "999.1.1.1:5" }) do
    ok("p1: refuses malformed address " .. vim.inspect(bad),
      (client.is_loopback(bad)) == false)
  end
end)()

-- ─────────────────── [2] client — error projection ───────────────────
print("\n[2] client.project_error — RAW wire slot to autodb's shape")
;(function()
  local client = require("autodb.client")

  -- The server's own shape.
  local p = client.project_error({ code = -32030, message = "invalid credentials" })
  ok("p2: projects the server's {code, message}",
    p.code == -32030 and p.message == "invalid credentials", vim.inspect(p))

  -- A peer that is not autodb at all: Neovim's [type, message] array.
  local q = client.project_error({ 0, "Vim:E121: Undefined variable" })
  ok("p2: projects an nvim-style [type, message] array",
    q.code == 0 and q.message:find("E121", 1, true) ~= nil, vim.inspect(q))

  ok("p2: passes a bare string through",
    client.project_error("boom").message == "boom")
  ok("p2: never returns nil for an unexpected shape",
    type(client.project_error(42).message) == "string",
    vim.inspect(client.project_error(42)))
  ok("p2: a nil error still yields a message",
    type(client.project_error(nil).message) == "string")
end)()

-- ─────────────────── [3] endpoint resolution ───────────────────────
print("\n[3] lifecycle.resolve_endpoint — the binary owns the answer")
;(function()
  local lc = require("autodb.lifecycle")
  local bin = plugin_root .. "/bin/autodb"
  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb — run `go build -o bin/autodb ./cmd/autodb`")
    return
  end

  -- No port configured: the local socket is the default rendezvous.
  local ep, eerr = lc.resolve_endpoint(bin, nil)
  ok("p3: resolves an endpoint", ep ~= nil, tostring(eerr))
  if ep then
    ok("p3: the default is a unix socket (nvim calls it 'pipe')",
      ep.mode == "pipe", vim.inspect(ep))
    ok("p3: and the address is a path, not host:port",
      ep.addr:sub(1, 1) == "/" and ep.addr:match(":%d+$") == nil, ep.addr)
  end

  -- A configured port opts into TCP. The plugin never decides this — it
  -- asks, so there is one resolver rather than two that drift.
  local tmp = vim.fn.tempname()
  vim.fn.mkdir(tmp, "p")
  local tcfg = tmp .. "/tcp.toml"
  vim.fn.writefile({ "[server]", "port = 7419", 'bind = "127.0.0.1"' }, tcfg)
  local tep = lc.resolve_endpoint(bin, tcfg)
  ok("p3: a configured port yields tcp host:port",
    tep and tep.mode == "tcp" and tep.addr == "127.0.0.1:7419", vim.inspect(tep))

  ok("p3: a missing binary is reported, not guessed",
    select(2, lc.resolve_endpoint(tmp .. "/nope", nil)) ~= nil)

  -- describe_manual always names the one command that always works.
  local msg = lc.describe_manual("nothing answered", { "bind: permission denied" })
  ok("p3: giving up names the manual fallback",
    msg:find("autodb --serve", 1, true) ~= nil, msg)
  ok("p3: and includes the detail it saw",
    msg:find("permission denied", 1, true) ~= nil, msg)
  vim.fn.delete(tmp, "rf")
end)()

-- ─────────────────── [4] end to end over a unix socket ─────────────
print("\n[4] client.connect — handshake over a REAL daemon on a socket")
;(function()
  local client = require("autodb.client")
  local lc = require("autodb.lifecycle")
  local bin = plugin_root .. "/bin/autodb"
  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb")
    return
  end

  -- Private everything: this must never touch the developer's daemon,
  -- socket or data.
  local tmp = vim.fn.tempname()
  vim.fn.mkdir(tmp, "p")
  local sock = tmp .. "/db.sock"
  local cfg = tmp .. "/config.toml"
  vim.fn.writefile({
    "[server]",
    'socket = "' .. sock .. '"',
    "[meta]",
    'engine = "sqlite"',
    'path = "' .. tmp .. '/meta.db"',
    "[tui]",
    'notes_dir = "' .. tmp .. '/notes"',
  }, cfg)

  local ep = lc.resolve_endpoint(bin, cfg)
  ok("p4: the configured socket path is honoured",
    ep and ep.mode == "pipe" and ep.addr == sock, vim.inspect(ep))

  local job = vim.fn.jobstart({ bin, "--serve", "--config", cfg })
  ok("p4: the test daemon started", job > 0, job)

  local function connect(opts, ms)
    local done, res, rerr = false, nil, nil
    client.connect(opts, function(c, e) done, res, rerr = true, c, e end)
    vim.wait(ms or 4000, function() return done end, 20)
    return res, rerr
  end

  local c, cerr
  vim.wait(8000, function()
    c, cerr = connect({ addr = sock, mode = "pipe" }, 700)
    return c ~= nil
  end, 250)

  ok("p4: connects over the socket and completes the handshake", c ~= nil, tostring(cerr))
  if c then
    ok("p4: no trust ceremony was needed — the socket IS the boundary",
      c:mode() == "pipe", c:mode())
    ok("p4: hello reported this protocol", c:hello().protocol == client.PROTOCOL,
      vim.inspect(c:hello() and c:hello().protocol))
    ok("p4: an instance id is recorded for epoch conditioning",
      type(c:instance()) == "string" and c:instance() ~= "", vim.inspect(c:instance()))
    ok("p4: hello no longer carries a launch nonce",
      c:hello().launch_nonce == nil, vim.inspect(c:hello().launch_nonce))
    ok("p4: the socket file is mode 0600 — the access control",
      vim.fn.getfperm(sock):sub(1, 6) == "rw----", vim.fn.getfperm(sock))
    c:close()
    ok("p4: close is reflected in is_ready", c:is_ready() == false)
  end

  -- A second frontend connects to the SAME daemon with no ceremony —
  -- which is the whole point of dropping the nonce.
  local c2 = connect({ addr = sock, mode = "pipe" })
  ok("p4: a second frontend connects to the same daemon freely", c2 ~= nil)
  if c2 then
    ok("p4: and sees the same server instance",
      c ~= nil and c2:instance() == c:instance(),
      tostring(c2:instance()) .. " vs " .. tostring(c and c:instance()))
    c2:close()
  end

  -- is_listening must probe, not stat: the file outlives the process.
  ok("p4: is_listening sees the live daemon",
    lc.is_listening({ mode = "pipe", addr = sock }) == true)
  pcall(vim.fn.jobstop, job)
  vim.wait(2000, function()
    return lc.is_listening({ mode = "pipe", addr = sock }) == false
  end, 100)
  ok("p4: and reports false once it is gone",
    lc.is_listening({ mode = "pipe", addr = sock }) == false)

  -- TCP still refuses non-loopback, even now that it is opt-in.
  local far, farerr = connect({ addr = "10.1.2.3:7419", mode = "tcp" }, 1500)
  ok("p4: a non-loopback TCP address is refused before dialing",
    far == nil and tostring(farerr):find("loopback", 1, true) ~= nil, tostring(farerr))

  vim.fn.delete(tmp, "rf")
end)()

print(string.format("\n%d passed, %d failed", pass_count, fail_count))
if fail_count > 0 then
  vim.cmd("cq!")
end
