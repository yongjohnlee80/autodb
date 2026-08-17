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

-- ─────────────────── [3] end to end against a real server ───────────
print("\n[3] client.connect — handshake and launch-proof, against a REAL daemon")
;(function()
  local client = require("autodb.client")
  local bin = plugin_root .. "/bin/autodb"
  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb — run `make build` for the end-to-end sections")
    return
  end

  -- A private port, config and meta store: this must never touch the
  -- developer's real daemon or their real data.
  local probe = vim.fn.serverstart("127.0.0.1:0")
  local port = tostring(probe):match(":(%d+)$")
  vim.fn.serverstop(probe)

  local tmp = vim.fn.tempname()
  vim.fn.mkdir(tmp, "p")
  local cfg = tmp .. "/config.toml"
  vim.fn.writefile({
    "[server]",
    "port = " .. port,
    'bind = "127.0.0.1"',
    "[meta]",
    'engine = "sqlite"',
    'path = "' .. tmp .. '/meta.db"',
    "[tui]",
    'notes_dir = "' .. tmp .. '/notes"',
  }, cfg)

  local nonce = "smoke-launch-nonce-" .. tostring(port)
  local noncefile = tmp .. "/launch.nonce"
  vim.fn.writefile({ nonce }, noncefile)
  vim.fn.setfperm(noncefile, "rw-------")

  local addr = "127.0.0.1:" .. port
  local job = vim.fn.jobstart({ bin, "--serve", "--config", cfg,
    "--launch-nonce-file", noncefile })
  ok("p3: the test daemon started", job > 0, job)

  local function connect(opts, ms)
    local done, res, rerr = false, nil, nil
    client.connect(opts, function(c, e) done, res, rerr = true, c, e end)
    vim.wait(ms or 5000, function() return done end, 20)
    return res, rerr
  end

  -- Wait for the port, retrying: the daemon binds a moment after spawn.
  local c, cerr
  vim.wait(6000, function()
    c, cerr = connect({ addr = addr, expect_nonce = vim.fn.sha256(nonce) }, 800)
    return c ~= nil
  end, 250)

  ok("p3: connects and completes the handshake", c ~= nil, tostring(cerr))
  if c then
    ok("p3: the nonce proved the daemon is OUR child", c:is_managed() == true)
    ok("p3: hello reported this protocol", c:hello().protocol == client.PROTOCOL,
      vim.inspect(c:hello() and c:hello().protocol))
    ok("p3: an instance id is recorded for epoch conditioning",
      type(c:instance()) == "string" and c:instance() ~= "", vim.inspect(c:instance()))
    ok("p3: the file's nonce was consumed by the daemon",
      vim.fn.filereadable(noncefile) == 0, "nonce file should be unlinked at startup")
    c:close()
    ok("p3: close is reflected in is_ready", c:is_ready() == false)
  end

  -- The refusal that matters: a daemon presenting the WRONG proof must
  -- never receive credentials, whatever pid it claims.
  local bad, baderr = connect({ addr = addr, expect_nonce = vim.fn.sha256("not-the-nonce") })
  ok("p3: a WRONG launch proof is refused", bad == nil and baderr ~= nil, tostring(baderr))
  ok("p3: and the refusal says why",
    tostring(baderr):find("launch proof", 1, true) ~= nil, baderr)

  -- No nonce recorded and no explicit trust: also refused, because the
  -- point is to ask BEFORE credentials flow.
  local ext, exterr = connect({ addr = addr })
  ok("p3: an unproved daemon is refused without explicit trust",
    ext == nil and exterr ~= nil, tostring(exterr))
  ok("p3: the refusal names the pid so the user can check it",
    tostring(exterr):find("pid", 1, true) ~= nil, exterr)

  -- With trust granted, the same daemon is usable and marked external.
  local trusted = connect({ addr = addr, trust_external = true })
  ok("p3: an explicitly trusted daemon connects", trusted ~= nil)
  if trusted then
    ok("p3: and is NOT marked as managed", trusted:is_managed() == false)
    trusted:close()
  end

  -- Non-loopback never dials, even with everything else in order.
  local far, farerr = connect({ addr = "10.1.2.3:" .. port, trust_external = true }, 1500)
  ok("p3: a non-loopback address is refused before dialing",
    far == nil and tostring(farerr):find("loopback", 1, true) ~= nil, tostring(farerr))

  pcall(vim.fn.jobstop, job)
  vim.fn.delete(tmp, "rf")
end)()

-- ─────────────────── [4] lifecycle — record and recovery ───────────
print("\n[4] lifecycle — nonce, spawn record, and stale resolution")
;(function()
  local lc = require("autodb.lifecycle")

  -- The nonce must be unguessable: math.random would be seeded
  -- predictably, and a guessable launch proof proves nothing.
  local n1 = lc.new_nonce()
  local n2 = lc.new_nonce()
  ok("p4: a nonce is 64 hex chars (32 bytes)",
    type(n1) == "string" and #n1 == 64 and n1:match("^%x+$") ~= nil, vim.inspect(n1))
  ok("p4: two nonces differ", n1 ~= n2)

  -- resolve_stale is the deciding half, split from the acting half so
  -- the decision — the part that must never guess — is testable.
  local now = 1000000
  local function res(rec, at) return lc.resolve_stale(rec, at or now) end

  local a, d = res({ state = "running", generation = "g" })
  ok("p4: a finalized record is adopted, not signalled", a == "adopt", a .. ": " .. d)

  a, d = res({ state = "tombstone", generation = "g" })
  ok("p4: a tombstoned generation is already resolved", a == "clean", a .. ": " .. d)

  -- Inside the deadline NOTHING is touched, even with no child pid yet:
  -- a launcher that has published its intent but not yet spawned is
  -- healthy, and cleaning it would wipe another instance's launch
  -- mid-flight.
  a, d = res({ state = "starting", generation = "g", deadline = now + 5000 })
  ok("p4: inside the deadline we wait, even with no child recorded",
    a == "wait", a .. ": " .. d)
  a, d = res({ state = "starting", generation = "g", deadline = now + 5000,
    child_pid = 999999999 })
  ok("p4: inside the deadline we wait even for a dead-looking child",
    a == "wait", a .. ": " .. d)

  a, d = res({ state = "starting", generation = "g", deadline = now - 1 })
  ok("p4: a starting record with NO child pid is cleaned, never signalled",
    a == "clean", a .. ": " .. d)

  a, d = res({ state = "starting", generation = "g", deadline = now - 1, child_pid = 999999999 })
  ok("p4: a dead child pid is cleaned", a == "clean", a .. ": " .. d)

  -- A pid that exists but is NOT ours: the pid-reuse hazard. This is
  -- the case that must never produce a kill.
  a, d = res({ state = "starting", generation = "g", deadline = now - 1,
    child_pid = vim.fn.getpid(), child_start = "999999999", exe = "/nonexistent/autodb" })
  ok("p4: a live pid that fails the identity check is NOT signalled",
    a == "clean", a .. ": " .. d)
  ok("p4: and the reason says why", d:find("not our child", 1, true) ~= nil, d)

  -- Only a positively identified child of ours is killable.
  local self_ident = lc.pid_identity(vim.fn.getpid())
  ok("p4: pid_identity reports start time and exe",
    self_ident ~= nil and self_ident.start ~= nil and self_ident.exe ~= nil,
    vim.inspect(self_ident))
  a, d = res({ state = "starting", generation = "g", deadline = now - 1,
    child_pid = vim.fn.getpid(), child_start = self_ident.start, exe = self_ident.exe })
  ok("p4: a fully identified child past its deadline may be killed",
    a == "kill", a .. ": " .. d)

  a, d = res({ state = "who-knows", generation = "g" })
  ok("p4: an unknown state REFUSES rather than guessing", a == "refuse", a .. ": " .. d)
  a, d = res("not a table")
  ok("p4: a non-table record refuses", a == "refuse", a .. ": " .. d)

  -- Refusing must be actionable: the developer's fallback is one command.
  local msg = lc.describe_manual("cannot identify the daemon", {
    generation = "g1", state = "starting", child_pid = 4242, address = "127.0.0.1:7419" })
  ok("p4: a refusal names the manual command",
    msg:find("autodb --serve", 1, true) ~= nil, msg)
  ok("p4: and the pid and address it found",
    msg:find("4242", 1, true) ~= nil and msg:find("127.0.0.1:7419", 1, true) ~= nil, msg)

  -- is_our_child needs ALL THREE of pid, start time and exe.
  ok("p4: is_our_child is false without a pid", lc.is_our_child({}) == false)
  ok("p4: is_our_child is false on a start-time mismatch",
    lc.is_our_child({ child_pid = vim.fn.getpid(), child_start = "0" }) == false)
  ok("p4: is_our_child is true for a full match",
    lc.is_our_child({ child_pid = vim.fn.getpid(), child_start = self_ident.start,
      exe = self_ident.exe }) == true)
end)()

print(string.format("\n%d passed, %d failed", pass_count, fail_count))
if fail_count > 0 then
  vim.cmd("cq!")
end
