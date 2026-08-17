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

-- ─────────────────── [5] binary discovery ──────────────────────────
print("\n[5] lifecycle.resolve_binary — found like gopls and delve are")
;(function()
  local lc = require("autodb.lifecycle")

  local cands = lc.binary_candidates(nil)
  ok("p5: candidates are ordered and labelled", #cands >= 2 and cands[1].label ~= nil,
    vim.inspect(vim.tbl_map(function(c) return c.label end, cands)))

  -- PATH before plugin-local: Mason prepends its bin dir to PATH inside
  -- Neovim, which is exactly how gopls and delve resolve by name alone.
  local labels = {}
  for i, c in ipairs(cands) do labels[i] = c.label end
  local joined = table.concat(labels, " | ")
  ok("p5: PATH is searched (covers Mason, go install, packages)",
    joined:find("PATH", 1, true) ~= nil, joined)
  ok("p5: a plugin-local build is searched (lazy `build` hook)",
    joined:find("plugin build", 1, true) ~= nil, joined)

  -- An explicit path always wins, and is never second-guessed.
  local first = lc.binary_candidates("/custom/autodb")[1]
  ok("p5: opts.bin takes priority over everything",
    first.path == "/custom/autodb" and first.label:find("configured", 1, true) ~= nil,
    vim.inspect(first))

  ok("p5: plugin_root resolves to this checkout",
    vim.fn.isdirectory(lc.plugin_root() .. "/lua/autodb") == 1, lc.plugin_root())

  -- A failure must list what it searched. "not found" alone is the least
  -- useful error a plugin can produce.
  -- An explicit opts.bin that is missing must FAIL, not fall through to
  -- some other autodb — running a different build than the one named is
  -- exactly the surprise this avoids.
  local cfg_bin, cfg_err = lc.resolve_binary("/definitely/not/here/autodb")
  ok("p5: a missing opts.bin fails instead of falling back",
    cfg_bin == nil and cfg_err ~= nil, tostring(cfg_bin))
  ok("p5: and the error names the path it was given",
    cfg_err and cfg_err:find("/definitely/not/here/autodb", 1, true) ~= nil, cfg_err)

  -- A search miss must list what it tried. "not found" alone is the
  -- least useful error a plugin can produce.
  local saved_path = vim.env.PATH
  vim.env.PATH = "/nonexistent-for-this-test"
  local orig_root = lc.plugin_root
  lc.plugin_root = function() return "/nonexistent-plugin-root" end
  local _, miss = lc.resolve_binary(nil)
  lc.plugin_root = orig_root
  vim.env.PATH = saved_path
  ok("p5: a search miss reports every path tried",
    miss and miss:find("PATH", 1, true) ~= nil
      and miss:find("plugin build", 1, true) ~= nil, miss)
  ok("p5: and names the ways to install it",
    miss and miss:find("Mason", 1, true) ~= nil
      and miss:find("go install", 1, true) ~= nil, miss)

  local bin = plugin_root .. "/bin/autodb"
  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb for the version comparison")
    return
  end

  local resolved, rerr, label = lc.resolve_binary(nil)
  ok("p5: resolves a real binary and says how", resolved ~= nil and label ~= nil,
    tostring(rerr) .. " " .. tostring(label))

  local v = lc.binary_version(bin)
  ok("p5: reads the binary's own version", type(v) == "string" and v ~= "", vim.inspect(v))

  -- The M6 footgun, made visible: a shared daemon outlives the frontend
  -- that started it BY DESIGN, so after a rebuild the old process keeps
  -- serving. This is what turns that into a message instead of an hour.
  local st_match = select(1, lc.build_status(v, bin))
  ok("p5: an identical build reports match", st_match == "match", st_match)
  local st_stale, msg_stale = lc.build_status("some-other-build", bin)
  ok("p5: a differing build reports stale", st_stale == "stale", st_stale)
  ok("p5: and the message says to restart",
    msg_stale:find("Restart", 1, true) ~= nil, msg_stale)
  ok("p5: no backend version is 'unknown', not a false match",
    (lc.build_status(nil, bin)) == "unknown")
end)()

-- ─────────────────── [6] stale-build notification ──────────────────
print("\n[6] lifecycle.check_build — tell the user, once, and name the key")
;(function()
  local lc = require("autodb.lifecycle")
  local keys = require("autodb.keys")
  local bin = plugin_root .. "/bin/autodb"

  ok("p6: the maintenance key is defined in one place",
    keys.MAINTENANCE == "<leader>DX", keys.MAINTENANCE)
  ok("p6: and every dbase key shares the prefix",
    keys.HISTORY:sub(1, #keys.PREFIX) == keys.PREFIX
    and keys.RUN_BUFFER:sub(1, #keys.PREFIX) == keys.PREFIX, keys.PREFIX)

  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb")
    return
  end

  -- Capture toasts on whichever path is live: autodb.log delegates to
  -- auto-core.log when it is present and falls back to vim.notify when
  -- it is not, so the test hooks the same branch the module takes
  -- rather than assuming one.
  local seen = {}
  local core_ok, core = pcall(require, "auto-core")
  local use_core = core_ok and core and core.log and type(core.log.notify) == "function"
  local real_notify, real_core = vim.notify, use_core and core.log.notify or nil
  local function capture(on)
    if on then
      if use_core then
        core.log.notify = function(msg, opts)
          seen[#seen + 1] = { msg = msg, lvl = (opts or {}).level }
        end
      else
        vim.notify = function(msg, lvl) seen[#seen + 1] = { msg = msg, lvl = lvl } end
      end
    else
      if use_core then core.log.notify = real_core else vim.notify = real_notify end
    end
  end
  local WARN = use_core and "warn" or vim.log.levels.WARN

  lc.forget_build_warnings()
  capture(true)
  local st = lc.check_build("some-other-build", bin)
  capture(false)

  ok("p6: a mismatch is reported as stale", st == "stale", st)
  ok("p6: the user is notified", #seen == 1, #seen)
  ok("p6: the message names the maintenance key",
    seen[1] and seen[1].msg:find(keys.MAINTENANCE, 1, true) ~= nil,
    seen[1] and seen[1].msg)
  ok("p6: and says to restart the backend",
    seen[1] and seen[1].msg:lower():find("restart", 1, true) ~= nil, seen[1] and seen[1].msg)
  ok("p6: at warning level", seen[1] and seen[1].lvl == WARN,
    seen[1] and tostring(seen[1].lvl))

  -- Once per mismatch, not once per reconnect: a warning that repeats
  -- on every connection is a warning people learn to ignore.
  seen = {}
  capture(true)
  lc.check_build("some-other-build", bin)
  lc.check_build("some-other-build", bin)
  capture(false)
  ok("p6: the same mismatch is not repeated", #seen == 0, #seen)

  -- A NEW mismatch still speaks up, and a restart clears the memory.
  seen = {}
  capture(true)
  lc.check_build("a-third-build", bin)
  capture(false)
  ok("p6: a different mismatch is reported", #seen == 1, #seen)

  seen = {}
  lc.forget_build_warnings()
  capture(true)
  lc.check_build("some-other-build", bin)
  capture(false)
  ok("p6: forgetting (a restart) lets it warn again", #seen == 1, #seen)

  -- A MATCHING build must stay silent.
  seen = {}
  local disk = lc.binary_version(bin)
  lc.forget_build_warnings()
  capture(true)
  local st_ok = lc.check_build(disk, bin)
  capture(false)
  ok("p6: a matching build notifies nothing", st_ok == "match" and #seen == 0,
    st_ok .. " notifications=" .. #seen)
end)()

-- ─────────────────── [7] commands + results ────────────────────────
print("\n[7] commands — the <leader>D surface, and results on the grid")
;(function()
  local commands = require("autodb.commands")
  local results = require("autodb.results")
  local keys = require("autodb.keys")

  require("autodb").setup()
  for _, lhs in ipairs({ keys.RUN_BUFFER, keys.CONNECTION, keys.MAINTENANCE, keys.HISTORY }) do
    ok("p7: " .. lhs .. " is bound", vim.fn.maparg(lhs, "n") ~= "", lhs)
  end
  ok("p7: the visual runner is bound in VISUAL mode",
    vim.fn.maparg(keys.RUN_VISUAL, "v") ~= "", keys.RUN_VISUAL)
  for _, cmd in ipairs({ "AutodbRun", "AutodbConnection", "AutodbHistory", "AutodbMaintenance" }) do
    ok("p7: :" .. cmd .. " exists", vim.fn.exists(":" .. cmd) == 2, cmd)
  end

  -- Requirement 9: <leader>Dr runs SQL files. Filetype OR extension, so
  -- a fresh scratch buffer is not refused over an autocmd race.
  local b1 = vim.api.nvim_create_buf(false, true)
  vim.bo[b1].filetype = "sql"
  ok("p7: a sql filetype counts", commands.is_sql(b1) == true)
  local b2 = vim.api.nvim_create_buf(false, true)
  vim.api.nvim_buf_set_name(b2, vim.fn.tempname() .. "/noname.sql")
  ok("p7: a .sql name counts even before the filetype is set",
    commands.is_sql(b2) == true)
  local b3 = vim.api.nvim_create_buf(false, true)
  ok("p7: a plain scratch buffer does not", commands.is_sql(b3) == false)

  -- The wire shape maps onto the grid model without translation.
  local model = results.from_wire({
    verb = "select", columns = { "id", "name" },
    rows = { { 1, "alpha" }, { 2, vim.NIL } },
    affected = 0, duration_ms = 12, more = false,
  })
  ok("p7: an exec reply becomes a grid model",
    model:nrows() == 2 and model:ncols() == 2, model:nrows() .. "x" .. model:ncols())
  ok("p7: values arrive RAW, display is derived",
    model:cell(1, 1).value == 1 and model:cell(2, 2).null == true,
    vim.inspect(model:cell(2, 2)))
  ok("p7: the summary carries verb and duration",
    model:summary():find("SELECT", 1, true) ~= nil
      and model:summary():find("12ms", 1, true) ~= nil, model:summary())

  -- An error is a RESULT STATE, not an absence of one.
  local emodel = results.from_error({ code = -32032, message = "syntax error near \"slect\"" })
  ok("p7: a failure renders as an error result",
    emodel:kind() == "error" and emodel:summary():find("slect", 1, true) ~= nil,
    emodel:summary())

  -- The panel is a bottom split that does not steal focus.
  local before_win = vim.api.nvim_get_current_win()
  local view = results.show(model)
  ok("p7: the result panel opened", view ~= nil and results.window() ~= nil)
  ok("p7: focus stayed in the query buffer",
    vim.api.nvim_get_current_win() == before_win)
  if results.window() then
    ok("p7: the result window is height-fixed",
      vim.api.nvim_get_option_value("winfixheight",
        { win = results.window(), scope = "local" }) == true)
    local lines = vim.api.nvim_buf_get_lines(view:buf(), 0, -1, false)
    ok("p7: the rows are rendered", #lines == 2 and lines[1]:match("^1%s+alpha") ~= nil,
      vim.inspect(lines))
  end

  -- Showing again replaces the view rather than stacking one per query.
  local first_buf = view:buf()
  local view2 = results.show(results.from_wire({ verb = "update", affected = 3 }))
  ok("p7: a second result reuses the window",
    view2 ~= nil and results.window() ~= nil)
  ok("p7: and disposes the previous view", not vim.api.nvim_buf_is_valid(first_buf))
  results.close()
  ok("p7: close tears the panel down", results.window() == nil)

  vim.api.nvim_buf_delete(b1, { force = true })
  vim.api.nvim_buf_delete(b2, { force = true })
  vim.api.nvim_buf_delete(b3, { force = true })
end)()

-- ─────────────────── [8] a query, end to end ───────────────────────
print("\n[8] a real query through the whole stack")
;(function()
  local bin = plugin_root .. "/bin/autodb"
  if vim.fn.executable(bin) ~= 1 then
    print("  SKIP  no bin/autodb")
    return
  end
  local client = require("autodb.client")
  local session = require("autodb.session")
  local results = require("autodb.results")

  local tmp = vim.fn.tempname()
  vim.fn.mkdir(tmp, "p")
  local sock = tmp .. "/db.sock"
  local cfg = tmp .. "/config.toml"
  vim.fn.writefile({
    "[server]", 'socket = "' .. sock .. '"',
    "[meta]", 'engine = "sqlite"', 'path = "' .. tmp .. '/meta.db"',
    "[tui]", 'notes_dir = "' .. tmp .. '/notes"',
  }, cfg)
  local job = vim.fn.jobstart({ bin, "--serve", "--config", cfg })

  local c
  vim.wait(8000, function()
    local done = false
    client.connect({ addr = sock, mode = "pipe" }, function(cl) c = cl; done = true end)
    vim.wait(700, function() return done end, 20)
    return c ~= nil
  end, 250)
  ok("p8: connected to a private daemon", c ~= nil)

  if c then
    -- Bootstrap an admin, then drive a real statement through the same
    -- path a command would: authed call, wire reply, grid model.
    local booted, berr = nil, nil
    c:call("auth.bootstrap", { "smoke-admin", "smoke-passphrase-1234" }, function(res, e)
      booted, berr = res, e
    end)
    vim.wait(5000, function() return booted ~= nil or berr ~= nil end, 20)
    ok("p8: bootstrapped the first admin", booted ~= nil, vim.inspect(berr))

    local token = type(booted) == "table" and (booted.token or booted[1]) or booted
    ok("p8: bootstrap returned a session token", type(token) == "string", type(token))
    if type(token) == "string" then
      c._token = token
      session.attach(c, { bin = bin })
      ok("p8: the session adopted the client", session.is_ready() == true)

      -- A statement that needs no connection: ask who we are.
      local who, werr = nil, nil
      c:authed("auth.whoami", {}, function(r, e) who, werr = r, e end)
      vim.wait(4000, function() return who ~= nil or werr ~= nil end, 20)
      ok("p8: an authenticated call round-trips", who ~= nil, vim.inspect(werr))
      ok("p8: and identifies the bootstrapped admin",
        who and vim.inspect(who):find("smoke-admin", 1, true) ~= nil, vim.inspect(who))

      -- workspace.list is what <leader>Dc drives.
      local spaces, serr = nil, nil
      c:authed("workspace.list", {}, function(r, e) spaces, serr = r or {}, e end)
      vim.wait(4000, function() return spaces ~= nil or serr ~= nil end, 20)
      ok("p8: workspace.list answers (the <leader>Dc source)",
        spaces ~= nil and serr == nil, vim.inspect(serr))

      -- And a failing statement must arrive as a projected error, not a
      -- raw wire value, so the panel has one shape to render.
      local ran, rerr = nil, nil
      c:authed("exec.run_script", { 999999, "SELECT 1" }, function(r, e) ran, rerr = r, e end)
      vim.wait(4000, function() return ran ~= nil or rerr ~= nil end, 20)
      ok("p8: a bad connection id fails with a projected error",
        rerr ~= nil and type(rerr.message) == "string", vim.inspect(rerr))
      local em = results.from_error(rerr)
      ok("p8: which the result panel renders as an error state",
        em:kind() == "error", em:summary())

      session.detach("smoke")
    end
    c:close()
  end
  pcall(vim.fn.jobstop, job)
  vim.fn.delete(tmp, "rf")
end)()

print(string.format("\n%d passed, %d failed", pass_count, fail_count))
if fail_count > 0 then
  vim.cmd("cq!")
end
