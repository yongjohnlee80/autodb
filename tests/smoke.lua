-- autodb.nvim — smoke test driver
--
-- Run headless from the repo root:
--   nvim --headless -u tests/smoke.lua -c 'qa!'
-- and prefer the runner (`tests/run-smoke.sh`, added by the Lua test-health
-- work) once it is on this branch — see the sentinel note below.
--
-- This suite is a documented EXCEPTION to the family convention's
-- `nvim --headless -u NONE -l <file>` rule, and the exception is load-bearing.
-- It must be SOURCED AS INIT (`-u tests/smoke.lua -c 'qa!'`) because its
-- sections drive real TUI grid rendering, which needs nvim's normal init/UI
-- path. Measured 2026-08-23 on the same tree:
--   * `-u tests/smoke.lua -c 'qa!'` → 209 passed, 0 failed, exit 0
--   * `-u NONE -l tests/smoke.lua`  → SIGABRT in nvim's own grid_line_flush
--                                     at §[11], 137 PASS, no summary, exit 134
-- So `-l` does not merely change the exit semantics here — it breaks the run.
--
-- The convention's REASON for mandating `-l` still applies: under `-c 'qa!'` an
-- uncaught throw is swallowed and nvim still quits EXIT 0, so an abort would
-- look green. That is why this suite must be driven by `tests/run-smoke.sh`,
-- which gates on the SMOKE-COMPLETE sentinel — the completion proof the
-- convention actually asks for. Never invoke this file bare.
-- (Family runner contract, shared/conventions/lua-nvim-plugin-development.md.)
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
--
-- The SAME-BRANCH sibling comes before `main`, because a cross-repo change
-- lives in two worktrees at once: developing ADR-0066 in
-- `autodb/grid-selection-detail` against `auto-core.nvim/main` would have
-- silently tested the frontend against an auto-core that has none of the
-- new primitives — and passed, by resolving `require` to the wrong copy.
-- That is the same failure mode as letting `runtimepath` shadow a checkout.
local siblings = vim.fn.fnamemodify(plugin_root, ":h:h")
local branch = vim.fn.fnamemodify(plugin_root, ":t")
local core_paths = {
  siblings .. "/auto-core.nvim/" .. branch,
  siblings .. "/auto-core.nvim/main",
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

-- float.multi drops its preview pane when the terminal is too narrow to
-- hold left + middle + preview (documented graceful degradation). A
-- headless nvim defaults to 80 columns, so the three-pane layout would
-- silently become two and the assertions would be testing the fallback.
vim.o.columns = 200
vim.o.lines = 50

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

-- A missing precondition is a FAILURE, not a silent skip.
--
-- Five sections drive a real `autodb --serve` and need bin/autodb, which is
-- gitignored (`git ls-files bin/` is empty) — so its ABSENCE is the default state
-- of a fresh clone or a misconfigured CI, not an edge case. When those sections
-- printed "SKIP" and returned, 45 assertions — including [4], the real-daemon
-- handshake and identity-refusal path this suite's own header calls "the security
-- boundary of this plugin" — vanished while the run still reported "0 failed" and
-- exited 0. A skip that cannot be told apart from a pass is not a skip. So an
-- absent binary is recorded here and made to fail the run at the summary, with the
-- one-line fix in the message.
-- A FLOOR on assertions run, because "assertions silently vanished" is exactly
-- the failure this suite exists to make loud (finding 1). If a future change drops
-- some — a mis-scoped skip, a section that stops executing — the total falls below
-- this and the run fails, even when nothing that DID run failed. Raise it when the
-- suite legitimately grows; never lower it to make a run pass.
local EXPECTED_MIN_ASSERTIONS = 335
local missing_prereqs = {}
local function require_bin(section)
  missing_prereqs[#missing_prereqs + 1] = section
  print(string.format("  MISSING  %s needs bin/autodb (gitignored) — build it: "
    .. "make build   (or: go build -o bin/autodb ./cmd/autodb)", section))
end

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
    require_bin("[3] resolve_endpoint")
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
    require_bin("[4] connect — real daemon handshake")
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
    require_bin("[5] resolve_binary")
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
    require_bin("[6] check_build")
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
    require_bin("[8] a real query through the whole stack")
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

-- ─────────────────── [8b] tx.status surface ────────────────────────
print("\n[8b] tx.status — the transaction-outcome poll surface (protocol 5)")
;(function()
  local tx = require("autodb.txstatus")

  -- Every state the outcome machine can produce has a glyph, and an
  -- unrecognised one reads as UNKNOWN rather than as success. A state this
  -- build has never heard of must not render like a clean commit -- that is
  -- the same defect audit v2 fixed on the history list, and it would be
  -- reintroduced here by a table with a permissive default.
  for _, st in ipairs({ "opened", "commit_started", "unknown_pending",
                        "committed", "rolled_back", "outcome_unresolvable" }) do
    local m = tx.state_mark(st)
    ok("p8b: " .. st .. " has a glyph", m ~= nil and m ~= "" and m ~= "?" or st == "outcome_unresolvable",
      tostring(m))
  end
  ok("p8b: an unknown state reads as unknown, not as success",
    tx.state_mark("some_future_state") == "?", tx.state_mark("some_future_state"))
  ok("p8b: a nil state reads as unknown", tx.state_mark(nil) == "?")
  ok("p8b: committed is NOT the unknown glyph",
    tx.state_mark("committed") ~= tx.state_mark("some_future_state"))

  -- How long it has been stuck is the number that decides whether to act, so
  -- it is rendered rather than left as two timestamps to subtract.
  ok("p8b: sub-second stays in ms", tx.stuck_for(340) == "340ms", tx.stuck_for(340))
  ok("p8b: seconds", tx.stuck_for(5000) == "5s", tx.stuck_for(5000))
  ok("p8b: minutes carry seconds", tx.stuck_for(125000) == "2m05s", tx.stuck_for(125000))
  ok("p8b: hours carry minutes", tx.stuck_for(3900000) == "1h05m", tx.stuck_for(3900000))
  ok("p8b: a nil duration is 0ms, not an error", tx.stuck_for(nil) == "0ms", tx.stuck_for(nil))

  -- A row renders without a reason, which is the ordinary case for a
  -- transaction that is merely open rather than stuck for a named cause.
  local line = tx.one_line({ state = "unknown_pending", conn_id = 3, reason = "timeout", stuck_ms = 90000 })
  ok("p8b: a pending row names the state, the connection and the age",
    line:find("unknown_pending", 1, true) ~= nil
      and line:find("conn 3", 1, true) ~= nil
      and line:find("timeout", 1, true) ~= nil
      and line:find("1m30s", 1, true) ~= nil, line)
  local bare = tx.one_line({ state = "opened", conn_id = 1, stuck_ms = 10 })
  ok("p8b: a row with no reason still renders", type(bare) == "string" and #bare > 0, bare)
  ok("p8b: an empty entry does not error", type(tx.one_line({})) == "string")
end)()

-- ─────────────────── [9] history modal ─────────────────────────────
print("\n[9] history — three panes over history.list (requirement 8)")
;(function()
  local hist = require("autodb.history")

  -- Formatting: the year is almost always this one and the UTC offset is
  -- never what you are scanning a list for.
  ok("p9: a timestamp becomes scannable",
    hist._fmt_when_for_tests("2026-08-18T09:14:03+09:00") == "08-18 09:14",
    hist._fmt_when_for_tests("2026-08-18T09:14:03+09:00"))
  ok("p9: an unparseable timestamp passes through rather than vanishing",
    hist._fmt_when_for_tests("not a date") == "not a date")

  -- A script is many lines; a list row is one.
  ok("p9: a multi-line script collapses for the list",
    hist._one_line_for_tests("select *\n  from t\n where x = 1")
      == "select * from t where x = 1",
    hist._one_line_for_tests("select *\n  from t\n where x = 1"))
  ok("p9: a nil script is empty, not an error",
    hist._one_line_for_tests(nil) == "")

  -- ADR-0074 §7 rev 2: every outcome gets its OWN glyph. This was a binary
  -- split -- error or not -- which was correct only while "not an error"
  -- could mean nothing but durable success. Audit v2 made that false, and a
  -- reader was then told an effect had landed when it had been discarded, or
  -- when nothing can ever establish whether it landed.
  local marks = {}
  for _, st in ipairs({ "ok", "running", "ok_pending_commit", "rolled_back",
                        "outcome_unresolvable", "error" }) do
    local m = hist.status_mark({ status = st })
    ok("p9: " .. st .. " has a glyph", m ~= nil and m ~= "", tostring(m))
    ok("p9: " .. st .. " is not confusable with durable success",
      st == "ok" or m ~= hist.STATUS_MARKS.ok, tostring(m))
    ok("p9: " .. st .. "'s glyph is unique", marks[m] == nil,
      string.format("%s collides with %s", st, tostring(marks[m])))
    marks[m] = st
  end
  -- An unknown status must read as unknown, never as fine: a state added on
  -- the daemon side must not arrive here looking like a success.
  ok("p9: an unrecognised status is not rendered as success",
    hist.status_mark({ status = "some_future_state" }) ~= hist.STATUS_MARKS.ok,
    hist.status_mark({ status = "some_future_state" }))
  -- A row carrying an error text is an error whatever its status says.
  ok("p9: an error text wins over the status word",
    hist.status_mark({ status = "ok", error = "boom" }) == hist.STATUS_MARKS.error)

  local entries = {
    { id = 1, connection = "analytics", user = "johno", script = "select 1",
      started_at = "2026-08-18T09:00:00+09:00", status = "ok", row_count = 1 },
    { id = 2, connection = "analytics", user = "johno", script = "select 2",
      started_at = "2026-08-18T09:05:00+09:00", status = "ok", row_count = 1 },
    { id = 3, connection = "billing", user = "ops", script = "update x set y=1",
      started_at = "2026-08-18T09:10:00+09:00", status = "error", error = "boom" },
    { id = 4, connection_id = 77, user = "ops", script = "select 4",
      started_at = "2026-08-18T09:20:00+09:00", status = "ok" },
  }

  -- The filter offers what the history CONTAINS, not every connection
  -- that exists, and busiest first so the useful one is nearest.
  local conns = hist._connections_for_tests(entries)
  ok("p9: the first filter entry is (all)",
    conns[1].label:find("all", 1, true) ~= nil and conns[1].key == nil,
    vim.inspect(conns[1]))
  ok("p9: (all) counts every entry", conns[1].count == 4, conns[1].count)
  ok("p9: connections are listed busiest first",
    conns[2].label == "analytics" and conns[2].count == 2, vim.inspect(conns[2]))
  ok("p9: a connection with no name falls back to its id",
    (function()
      for _, c in ipairs(conns) do
        if c.label == "#77" then return true end
      end
      return false
    end)(), vim.inspect(vim.tbl_map(function(c) return c.label end, conns)))

  -- Filtering by the selected connection.
  local state = { entries = entries, conns = conns, conn_idx = 1 }
  ok("p9: (all) filters nothing", #hist._filtered_for_tests(state) == 4)
  state.conn_idx = 2
  local only = hist._filtered_for_tests(state)
  ok("p9: selecting a connection narrows the list",
    #only == 2 and only[1].connection == "analytics", #only)

  ok("p9: is_open is false before opening", hist.is_open() == false)
  ok("p9: close on a closed modal is a no-op", pcall(hist.close))

  -- Opening needs a session; without one it must report rather than
  -- throw, because <leader>Dh is reachable at any time.
  local session = require("autodb.session")
  session.reset_for_tests()
  -- The report is an ERROR-level notify, and auto-core mirrors those to
  -- `vim.notify` from inside a `vim.schedule` (log.lua — dispatch can
  -- come from a libuv callback where notifying is unsafe). Capture it
  -- HERE rather than leaving a queued toast to surface during a later
  -- section's event-loop pump, where it prints "Error in <script>" while
  -- the suite reports green — a clean count over a dirty stderr is not a
  -- clean signal.
  local orig_notify = vim.notify
  local toasts = {}
  vim.notify = function(msg) toasts[#toasts + 1] = tostring(msg) end
  local opened_without_session = pcall(hist.open)
  vim.wait(500, function() return #toasts > 0 end, 5)
  vim.notify = orig_notify
  ok("p9: opening with no session does not error", opened_without_session)
  ok("p9: and says why rather than failing silently",
    toasts[1] ~= nil and toasts[1]:find("cannot list history", 1, true) ~= nil,
    tostring(toasts[1]))
  ok("p9: and stays closed", hist.is_open() == false)

  -- The float wiring itself, with the server stubbed. Helper functions
  -- passing is not evidence that three panes render and that the cursor
  -- drives the preview.
  local real_authed = session.authed
  session.authed = function(method, _params, cb)
    if method == "history.list" then return cb(entries, nil) end
    return cb(nil, { message = "unexpected " .. method })
  end
  local opened = pcall(hist.open)
  session.authed = real_authed

  ok("p9: it opens with entries", opened and hist.is_open() == true, tostring(opened))
  if hist.is_open() then
    local st = hist._state_for_tests()
    local panes = st.float.panes or {}
    ok("p9: three panes exist", panes.left and panes.middle and panes.preview,
      vim.inspect(vim.tbl_keys(panes)))

    -- Guarded: the assertion above can legitimately be false (a narrow terminal
    -- drops a pane), and dereferencing panes.left.bufnr then throws OUT of the
    -- chunk — which nvim exits 0 on, darkening every later section (finding 3). A
    -- missing pane must fail these assertions, not abort the suite.
    if panes.left then
      local left = vim.api.nvim_buf_get_lines(panes.left.bufnr, 0, -1, false)
      ok("p9: the left pane lists the filters",
        #left == #conns and left[1] and left[1]:find("all", 1, true) ~= nil, vim.inspect(left))
      ok("p9: the selected filter is marked",
        left[1] ~= nil and left[1]:find("▸", 1, true) ~= nil, left[1])
    else
      ok("p9: the left pane lists the filters", false, "no left pane to read")
      ok("p9: the selected filter is marked", false, "no left pane to read")
    end

    local mid = vim.api.nvim_buf_get_lines(panes.middle.bufnr, 0, -1, false)
    ok("p9: the middle pane lists every entry", #mid == #entries, #mid)
    ok("p9: a row carries date, user and a one-line script",
      mid[1]:find("09:00", 1, true) ~= nil and mid[1]:find("johno", 1, true) ~= nil
      and mid[1]:find("select 1", 1, true) ~= nil, mid[1])
    ok("p9: a failed statement is marked",
      (function()
        for _, l in ipairs(mid) do if l:find("✗", 1, true) then return true end end
        return false
      end)(), vim.inspect(mid))

    local prev = vim.api.nvim_buf_get_lines(panes.preview.bufnr, 0, -1, false)
    ok("p9: the preview shows the first entry in full",
      table.concat(prev, "\n"):find("select 1", 1, true) ~= nil, vim.inspect(prev))
    ok("p9: and its metadata", table.concat(prev, "\n"):find("connection:", 1, true) ~= nil)
    ok("p9: the script pane is treated as SQL",
      vim.bo[panes.preview.bufnr].filetype == "sql", vim.bo[panes.preview.bufnr].filetype)

    -- The cursor drives the preview: that is what makes it a browser.
    if panes.middle.winid and vim.api.nvim_win_is_valid(panes.middle.winid) then
      vim.api.nvim_win_set_cursor(panes.middle.winid, { 3, 0 })
      hist._preview(st)
      local p3 = table.concat(vim.api.nvim_buf_get_lines(panes.preview.bufnr, 0, -1, false), "\n")
      ok("p9: moving the cursor repaints the preview",
        p3:find("update x set y=1", 1, true) ~= nil, p3:sub(1, 120))
      ok("p9: and a failed entry shows its error", p3:find("boom", 1, true) ~= nil, p3:sub(1, 200))
    end

    -- Buffer-local keymaps, so nothing leaks into the editor.
    local maps = {}
    for _, m in ipairs(vim.api.nvim_buf_get_keymap(panes.middle.bufnr, "n")) do
      maps[m.lhs] = true
    end
    ok("p9: <CR>, y and R are bound on the entry pane",
      maps["<CR>"] and maps["y"] and maps["R"], vim.inspect(vim.tbl_keys(maps)))

    hist.close()
    ok("p9: closing releases the modal", hist.is_open() == false)
  end

  -- A narrow terminal DROPS the preview rather than failing — the
  -- documented behaviour of float.multi, and the reason _preview guards
  -- for a missing pane instead of assuming three.
  local wide = vim.o.columns
  vim.o.columns = 80
  session.authed = function(method, _params, cb)
    if method == "history.list" then return cb(entries, nil) end
    return cb(nil, { message = "unexpected " .. method })
  end
  local narrow_ok = pcall(hist.open)
  session.authed = real_authed
  ok("p9: a narrow terminal still opens", narrow_ok and hist.is_open() == true)
  if hist.is_open() then
    local np = hist._state_for_tests().float.panes or {}
    ok("p9: and degrades to two panes rather than erroring",
      np.left ~= nil and np.middle ~= nil and np.preview == nil,
      vim.inspect(vim.tbl_keys(np)))
    ok("p9: the entry list still renders",
      np.middle ~= nil
        and #vim.api.nvim_buf_get_lines(np.middle.bufnr, 0, -1, false) == #entries,
      "no middle pane to read")
    hist.close()
  end
  vim.o.columns = wide
end)()

-- ─────────────────── [10] refresh ──────────────────────────────────
print("\n[10] refresh — pull, rebuild, relaunch (and refuse when it cannot)")
;(function()
  local refresh = require("autodb.refresh")
  local lifecycle = require("autodb.lifecycle")
  local commands = require("autodb.commands")

  ok("p10: refresh is reachable from the maintenance prompt",
    type(commands.refresh) == "function")

  -- Drive the success path with the plugin-local binary EXPLICITLY.
  --
  -- resolve_binary lists PATH/Mason candidates ahead of the plugin build, so on
  -- any machine where autodb is installed (Mason, `go install`) a bare
  -- refresh.preflight({}) resolves the Mason binary, refuses, and this success
  -- path SKIPped — i.e. it was untested precisely where autodb is actually
  -- installed (finding 2). opts.bin is honoured when executable (lifecycle.lua),
  -- so passing the plugin build reaches the success path on every machine.
  local plugin_bin = lifecycle.plugin_root() .. "/bin/" .. lifecycle.BINARY_NAME
  if vim.fn.executable(plugin_bin) == 1 then
    local pass_ok, pass_err, plan = refresh.preflight({ bin = plugin_bin })
    ok("p10: preflight passes for the plugin-local build", pass_ok == true, tostring(pass_err))
    ok("p10: and reports the checkout it would pull",
      plan and plan.root == lifecycle.plugin_root(), plan and (plan.root or "no plan"))
  else
    -- The binary is gitignored, so its absence is a required precondition, not a
    -- skip (finding 1) — the same treatment as the five end-to-end sections.
    require_bin("[10] refresh preflight success path")
  end

  -- The guard that matters: if the binary came from PATH (Mason, go
  -- install), refreshing THIS checkout changes nothing about the
  -- executable that runs. Refusing and naming the right tool beats
  -- appearing to work.
  local tmp = vim.fn.tempname()
  vim.fn.mkdir(tmp, "p")
  local fake = tmp .. "/autodb"
  vim.fn.writefile({ "#!/bin/sh", "echo autodb dev" }, fake)
  vim.fn.setfperm(fake, "rwxr-xr-x")

  local ok_other, err_other = refresh.preflight({ bin = fake })
  ok("p10: refuses when the binary is not the plugin build", ok_other == false)
  ok("p10: and names Mason and go install as the right tools",
    err_other and err_other:find("Mason", 1, true) ~= nil
    and err_other:find("go install", 1, true) ~= nil, err_other)
  ok("p10: and points at restart for picking it up",
    err_other and err_other:find(require("autodb.keys").MAINTENANCE, 1, true) ~= nil,
    err_other)

  -- A non-git checkout has no branch to fetch, and should say that
  -- rather than shelling out and relaying git's exit code.
  local orig_root = lifecycle.plugin_root
  lifecycle.plugin_root = function() return tmp end
  local nogit_ok, nogit_err = refresh.preflight({ bin = fake })
  lifecycle.plugin_root = orig_root
  ok("p10: a non-git checkout is refused with the manual command",
    nogit_ok == false and nogit_err ~= nil, nogit_err)

  -- Build goes through the Makefile, which owns -buildvcs=false AND the
  -- ldflags version stamp that the stale-backend check reads. A second
  -- recipe here would drift and quietly report `dev` forever.
  local mk = vim.fn.readfile(lifecycle.plugin_root() .. "/Makefile")
  local mk_text = table.concat(mk, "\n")
  ok("p10: the Makefile still owns the version stamp",
    mk_text:find("main.version", 1, true) ~= nil, "ldflags present")
  ok("p10: and the buildvcs workaround",
    mk_text:find("buildvcs=false", 1, true) ~= nil)

  vim.fn.delete(tmp, "rf")
end)()

-- ──────────── [11] login — masked, and retryable after a slip ────────────
print("\n[11] login — the passphrase is masked, and a failure can be retried")
;(function()
  local autodb = require("autodb")
  local session = require("autodb.session")
  local commands = require("autodb.commands")
  local keys = require("autodb.keys")

  -- A stub client is enough here, and a real daemon would be worse:
  -- `_login` only ever touches `c:call` and `c._token`, and what needs
  -- proving is which PROMPT the passphrase travelled through — something
  -- no server can observe. Section [8] already covers real auth.
  local function fake_client(opts)
    opts = opts or {}
    return {
      _token = nil,
      calls = {},
      is_ready = function() return true end,
      token = function(self) return self._token end,
      call = function(self, method, params, cb)
        self.calls[#self.calls + 1] = method
        if method == "auth.needs_bootstrap" then
          return cb(opts.needs_bootstrap == true, nil)
        end
        if method == "auth.login" or method == "auth.bootstrap" then
          self.sent = params
          if opts.reject then return cb(nil, { message = "invalid passphrase" }) end
          return cb({ token = "tok-1" }, nil)
        end
        return cb(nil, { message = "unexpected " .. method })
      end,
    }
  end

  -- `_login` refuses to prompt without a UI, so a headless run has to
  -- claim one. `echoed` collects prompts that render keystrokes;
  -- `masked` collects prompts that do not. The passphrase must only ever
  -- appear in the second list.
  local orig_uis, orig_input, orig_secret =
    vim.api.nvim_list_uis, vim.ui.input, vim.fn.inputsecret
  local echoed, masked = {}, {}
  local function capture(answers)
    vim.api.nvim_list_uis = function() return { { chan = 1 } } end
    vim.ui.input = function(o, cb) echoed[#echoed + 1] = o.prompt; cb(answers.name) end
    vim.fn.inputsecret = function(p) masked[#masked + 1] = p; return answers.pass end
  end
  local function restore()
    vim.api.nvim_list_uis = orig_uis
    vim.ui.input = orig_input
    vim.fn.inputsecret = orig_secret
  end
  -- The masked prompt is deferred onto a settled main loop, so the stubs
  -- must stay installed until it has run.
  local function settle(probe)
    vim.wait(2000, probe, 5)
  end

  -- ── first run: bootstrap ──
  echoed, masked = {}, {}
  capture({ name = "root", pass = "s3cret" })
  local c1 = fake_client({ needs_bootstrap = true })
  local ok1, err1
  autodb._login(c1, function(o, e) ok1, err1 = o, e end)
  settle(function() return ok1 ~= nil end)
  restore()

  ok("p11: a first run prompts to CREATE the admin",
    echoed[1] ~= nil and echoed[1]:find("create admin user", 1, true) ~= nil, echoed[1])
  ok("p11: and bootstraps rather than logging in",
    vim.tbl_contains(c1.calls, "auth.bootstrap"), table.concat(c1.calls, ","))
  ok("p11: the passphrase goes through a MASKED prompt", #masked == 1, tostring(#masked))
  ok("p11: named so the user knows what is being asked",
    masked[1] ~= nil and masked[1]:find("passphrase", 1, true) ~= nil, masked[1])
  ok("p11: and the echoing prompt is used ONCE — for the name only",
    #echoed == 1, tostring(#echoed))
  ok("p11: bootstrap kept the token", ok1 == true and c1._token ~= nil, tostring(err1))

  -- ── returning user: login ──
  echoed, masked = {}, {}
  capture({ name = "tester", pass = "s3cret" })
  local c2 = fake_client({ needs_bootstrap = false })
  local ok2 = nil
  autodb._login(c2, function(o) ok2 = o end)
  settle(function() return ok2 ~= nil end)
  restore()
  ok("p11: an existing store asks for a user, not a new admin",
    echoed[1] ~= nil and echoed[1]:find("create", 1, true) == nil, echoed[1])
  ok("p11: and logs in", ok2 == true and vim.tbl_contains(c2.calls, "auth.login"),
    table.concat(c2.calls, ","))
  ok("p11: the passphrase is still masked on the login path", #masked == 1, tostring(#masked))

  -- ── cancelling ──
  echoed, masked = {}, {}
  capture({ name = "root", pass = "" })
  local c3 = fake_client({ needs_bootstrap = false })
  local ok3, err3
  autodb._login(c3, function(o, e) ok3, err3 = o, e end)
  settle(function() return ok3 ~= nil end)
  restore()
  ok("p11: an empty passphrase cancels",
    ok3 == false and tostring(err3):find("cancelled", 1, true) ~= nil, tostring(err3))
  ok("p11: and no credentials leave the editor",
    not vim.tbl_contains(c3.calls, "auth.login"), table.concat(c3.calls, ","))

  -- ── a wrong passphrase ──
  echoed, masked = {}, {}
  capture({ name = "root", pass = "wrong" })
  local c4 = fake_client({ needs_bootstrap = false, reject = true })
  local ok4, err4
  autodb._login(c4, function(o, e) ok4, err4 = o, e end)
  settle(function() return ok4 ~= nil end)
  restore()
  ok("p11: a rejected passphrase surfaces the server's reason",
    ok4 == false and tostring(err4):find("invalid passphrase", 1, true) ~= nil, tostring(err4))
  ok("p11: and leaves no token behind", c4._token == nil)

  -- ── the wedge: connected is not signed in ──
  -- Before this section existed, a mistyped passphrase left a client
  -- that was `is_ready()` but token-less, so every later command sailed
  -- past the login prompt and answered "not logged in" instead. One slip
  -- wedged the session for the rest of the Neovim run.
  session.reset_for_tests()
  local c5 = fake_client({ needs_bootstrap = false })
  session.attach(c5, {})
  ok("p11: a connected client with NO token is not session-ready",
    session.is_ready() == false)
  c5._token = "tok-1"
  ok("p11: and becomes ready once the token lands", session.is_ready() == true)

  -- ── the retry is a login, not a second daemon ──
  session.reset_for_tests()
  local c6 = fake_client({ needs_bootstrap = false })
  session.attach(c6, {})
  local lifecycle = require("autodb.lifecycle")
  local orig_resolve, orig_spawn = lifecycle.resolve_binary, lifecycle.spawn
  local reached_lifecycle = false
  lifecycle.resolve_binary = function()
    reached_lifecycle = true
    return nil, "resolve_binary must not run when a client is already live"
  end
  lifecycle.spawn = function() reached_lifecycle = true end
  echoed, masked = {}, {}
  capture({ name = "root", pass = "s3cret" })
  local ok6, err6
  autodb.ensure_connected(function(o, e) ok6, err6 = o, e end)
  settle(function() return ok6 ~= nil end)
  restore()
  lifecycle.resolve_binary, lifecycle.spawn = orig_resolve, orig_spawn
  ok("p11: a retry logs in on the live client", ok6 == true, tostring(err6))
  ok("p11: without resolving a binary or spawning a daemon", reached_lifecycle == false)
  ok("p11: and it reached auth.login", vim.tbl_contains(c6.calls, "auth.login"),
    table.concat(c6.calls, ","))

  -- ── one prompt at a time, but never a stranded guard ──
  -- Two commands in quick succession must not open two passphrase
  -- prompts. The guard cannot be absolute though: a `vim.ui.input`
  -- handler that drops its callback on cancel would hold it forever and
  -- refuse every later attempt, which is the same one-slip wedge this
  -- section exists to prevent. So the automatic path defers, and a
  -- deliberate <leader>Dl press replaces the prompt.
  echoed, masked = {}, {}
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.input = function(o) echoed[#echoed + 1] = o.prompt end   -- never answers
  vim.fn.inputsecret = function(p) masked[#masked + 1] = p; return "s3cret" end
  local c7 = fake_client({ needs_bootstrap = false })
  autodb._login(c7, function() end)                 -- leaves a prompt open
  local ok7, err7
  autodb._login(c7, function(o, e) ok7, err7 = o, e end)
  ok("p11: a second automatic login defers to the open prompt",
    ok7 == false and tostring(err7):find("already open", 1, true) ~= nil, tostring(err7))
  ok("p11: and no second prompt was opened", #echoed == 1, tostring(#echoed))

  local ok8
  autodb._login(c7, function(o) ok8 = o end, { force = true })
  ok("p11: a deliberate press is never refused by a stale guard",
    ok8 == nil or ok8 ~= false, tostring(ok8))
  ok("p11: and it does open a prompt", #echoed == 2, tostring(#echoed))
  -- Clear the guard the abandoned prompts left set.
  vim.ui.input = function(o, cb) echoed[#echoed + 1] = o.prompt; cb(nil) end
  autodb._login(c7, function() end, { force = true })
  restore()

  -- ── the key surface ──
  ok("p11: " .. tostring(keys.LOGIN) .. " is the login key",
    keys.LOGIN == keys.PREFIX .. "l", tostring(keys.LOGIN))
  ok("p11: commands.login exists", type(commands.login) == "function")
  commands.setup({})
  local mapped = false
  for _, m in ipairs(vim.api.nvim_get_keymap("n")) do
    if m.desc and m.desc:find("autodb: sign in", 1, true) then mapped = true end
  end
  ok("p11: and setup binds it", mapped == true)

  session.reset_for_tests()
end)()

-- ─────────── [12] workspaces — <leader>Dw selects or creates ───────────
print("\n[12] workspaces — <leader>Dw lists, and can create (requirement 5)")
;(function()
  local session = require("autodb.session")
  local commands = require("autodb.commands")
  local keys = require("autodb.keys")

  -- A client whose `authed` answers the two workspace verbs. choose_
  -- workspace is pure client + session wiring; a real daemon would only
  -- retest what section [8] already covers over the wire.
  local function ws_client(opts)
    opts = opts or {}
    return {
      _token = "tok-1",
      calls = {},
      created = nil,
      is_ready = function() return true end,
      token = function(self) return self._token end,
      hello = function() return nil end,
      authed = function(self, method, params, cb)
        self.calls[#self.calls + 1] = method
        if method == "workspace.list" then
          return cb(opts.spaces or {}, nil)
        end
        if method == "workspace.create" then
          self.created = params[1]
          return cb(opts.new_id or 7, nil)
        end
        return cb(nil, { message = "unexpected " .. method })
      end,
      close = function() end,
    }
  end

  local orig_uis, orig_input, orig_select =
    vim.api.nvim_list_uis, vim.ui.input, vim.ui.select
  local function restore()
    vim.api.nvim_list_uis = orig_uis
    vim.ui.input = orig_input
    vim.ui.select = orig_select
  end
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end

  -- ── empty store → straight to create, no select prompt ──
  session.reset_for_tests()
  local c1 = ws_client({ spaces = {} })
  session.attach(c1, {})
  local selected_shown = false
  vim.ui.select = function() selected_shown = true end
  vim.ui.input = function(o, cb) cb("Analytics") end
  local got1
  commands.choose_workspace(function(ws) got1 = ws end)
  restore()
  ok("p12: an empty store skips the picker and prompts to create",
    selected_shown == false)
  ok("p12: workspace.create was called with the typed name",
    c1.created == "Analytics", tostring(c1.created))
  ok("p12: the new workspace becomes the active one",
    session.workspace() ~= nil and session.workspace().name == "Analytics",
    vim.inspect(session.workspace()))
  ok("p12: and the command handed it back", got1 ~= nil and got1.id == 7,
    vim.inspect(got1))

  -- ── existing workspaces → picker offers them + a Create row ──
  session.reset_for_tests()
  local spaces = {
    { id = 1, name = "LabelManager", connections = { { id = 9, name = "pg" } } },
    { id = 2, name = "AutoDB", connections = {} },
  }
  local c2 = ws_client({ spaces = spaces })
  session.attach(c2, {})
  local labels, captured_items
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.select = function(items, o, cb)
    captured_items = items
    labels = {}
    for _, it in ipairs(items) do labels[#labels + 1] = o.format_item(it) end
    cb(items[1])   -- pick the first existing workspace
  end
  local got2
  commands.choose_workspace(function(ws) got2 = ws end)
  restore()
  ok("p12: the picker lists every workspace plus one Create row",
    captured_items ~= nil and #captured_items == #spaces + 1, tostring(captured_items and #captured_items))
  ok("p12: the extra row reads as Create",
    labels and labels[#labels]:find("Create", 1, true) ~= nil, labels and labels[#labels])
  ok("p12: picking an existing workspace selects it without creating",
    got2 ~= nil and got2.name == "LabelManager" and c2.created == nil,
    vim.inspect(got2))
  ok("p12: a workspace's connection count is shown",
    labels and labels[1]:find("1 connection", 1, true) ~= nil, labels and labels[1])

  -- ── choosing the Create row from a non-empty list ──
  session.reset_for_tests()
  local c3 = ws_client({ spaces = spaces, new_id = 3 })
  session.attach(c3, {})
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.select = function(items, o, cb) cb(items[#items]) end  -- the Create row
  vim.ui.input = function(o, cb) cb("Scratch") end
  local got3
  commands.choose_workspace(function(ws) got3 = ws end)
  restore()
  ok("p12: the Create row leads to workspace.create",
    c3.created == "Scratch" and got3 ~= nil and got3.id == 3, tostring(c3.created))

  -- ── an empty / cancelled name creates nothing ──
  session.reset_for_tests()
  local c4 = ws_client({ spaces = {} })
  session.attach(c4, {})
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.input = function(o, cb) cb("") end
  local got4 = "sentinel"
  commands.choose_workspace(function(ws) got4 = ws end)
  restore()
  ok("p12: an empty name creates nothing and cancels",
    c4.created == nil and got4 == nil, tostring(c4.created))

  -- ── the key surface ──
  ok("p12: " .. tostring(keys.WORKSPACE) .. " is the workspace key",
    keys.WORKSPACE == keys.PREFIX .. "w", tostring(keys.WORKSPACE))
  ok("p12: commands.choose_workspace exists", type(commands.choose_workspace) == "function")
  commands.setup({})
  local mapped = false
  for _, m in ipairs(vim.api.nvim_get_keymap("n")) do
    if m.desc and m.desc:find("choose or create a workspace", 1, true) then mapped = true end
  end
  ok("p12: and setup binds it", mapped == true)
  ok("p12: the workspaces topic is registered",
    type(session.TOPIC_WORKSPACES) == "string" and type(session.workspaces_changed) == "function")

  session.reset_for_tests()
end)()

-- ─────────── [13] connections — <leader>Dc selects or creates ───────────
print("\n[13] connections — <leader>Dc lists, or creates and attaches")
;(function()
  local session = require("autodb.session")
  local commands = require("autodb.commands")

  local function cx(opts)
    opts = opts or {}
    return {
      _token = "tok-1",
      calls = {},          -- ordered method names
      created = nil,       -- {name, engine, dsn}
      attached = nil,      -- {ws_id, conn_id}
      is_ready = function() return true end,
      token = function(self) return self._token end,
      hello = function() return nil end,
      authed = function(self, method, params, cb)
        self.calls[#self.calls + 1] = method
        if method == "workspace.list" then return cb(opts.spaces or {}, nil) end
        if method == "conn.create" then
          self.created = { name = params[1], engine = params[2], dsn = params[3] }
          return cb(opts.new_id or 42, nil)
        end
        if method == "workspace.attach" then
          self.attached = { ws_id = params[1], conn_id = params[2] }
          return cb(nil, nil)
        end
        return cb(nil, { message = "unexpected " .. method })
      end,
      close = function() end,
    }
  end

  local orig_uis, orig_input, orig_select =
    vim.api.nvim_list_uis, vim.ui.input, vim.ui.select
  local function restore()
    vim.api.nvim_list_uis = orig_uis
    vim.ui.input = orig_input
    vim.ui.select = orig_select
  end
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end

  -- A programmable UI: inputs answered by prompt substring; selects
  -- answered by a chooser that sees the items and options.
  local function drive(inputs, on_select)
    vim.ui.input = function(o, cb)
      for pat, val in pairs(inputs) do
        if o.prompt:find(pat, 1, true) then return cb(val) end
      end
      return cb(nil)
    end
    vim.ui.select = function(items, o, cb) return cb(on_select(items, o)) end
  end

  -- ── empty workspace → straight to create+attach ──
  session.reset_for_tests()
  local c1 = cx({ spaces = { { id = 5, name = "AutoDB", connections = {} } }, new_id = 42 })
  session.attach(c1, {})
  local engine_options
  drive(
    { ["connection name"] = "local-pg", ["dsn"] = "postgres://u:p@localhost/db" },
    function(items, o)
      -- The only select in this flow is the engine picker.
      engine_options = items
      return "postgres"
    end)
  local got1
  commands.choose_connection(function(conn) got1 = conn end)
  restore()

  ok("p13: an empty workspace goes straight to create",
    c1.created ~= nil, vim.inspect(c1.created))
  ok("p13: conn.create carried name/engine/dsn",
    c1.created and c1.created.name == "local-pg" and c1.created.engine == "postgres"
    and c1.created.dsn == "postgres://u:p@localhost/db", vim.inspect(c1.created))
  ok("p13: the engine choice is a select over the three engines",
    engine_options ~= nil and #engine_options == 3
    and vim.tbl_contains(engine_options, "postgres")
    and vim.tbl_contains(engine_options, "mysql")
    and vim.tbl_contains(engine_options, "sqlite"), vim.inspect(engine_options))
  ok("p13: the new connection was attached to the workspace",
    c1.attached ~= nil and c1.attached.ws_id == 5 and c1.attached.conn_id == 42,
    vim.inspect(c1.attached))
  ok("p13: create is ordered before attach",
    c1.calls[#c1.calls - 1] == "conn.create" and c1.calls[#c1.calls] == "workspace.attach",
    table.concat(c1.calls, ","))
  ok("p13: the new connection becomes active",
    session.connection() ~= nil and session.connection().name == "local-pg",
    vim.inspect(session.connection()))

  -- ── create succeeds, ATTACH fails ──
  --
  -- A connection that exists but is not attached is a real failure and
  -- must reach the caller as one. It used to collapse to a bare nil,
  -- which autodb.api then reported as `cancelled` — telling a caller the
  -- user had backed out when the daemon had actually refused.
  session.reset_for_tests()
  local cfail = cx({ spaces = { { id = 5, name = "AutoDB", connections = {} } }, new_id = 42 })
  local inner_authed = cfail.authed
  cfail.authed = function(self, method, params, cb)
    if method == "workspace.attach" then
      self.calls[#self.calls + 1] = method
      return cb(nil, { message = "attach refused by the daemon" })
    end
    return inner_authed(self, method, params, cb)
  end
  session.attach(cfail, {})
  drive({ ["connection name"] = "orphan", ["dsn"] = "postgres://u:p@localhost/db" },
    function() return "postgres" end)
  local api = require("autodb.api")
  local fired, calls, fok, fval = false, 0, nil, nil
  api.choose_connection(function(o, v) fired, calls, fok, fval = true, calls + 1, o, v end)
  restore()
  ok("p13: a failed attach calls back exactly once", fired and calls == 1, tostring(calls))
  ok("p13: ...reporting failure, not success", fired and fok == false, tostring(fok))
  ok("p13: ...coded daemon, NOT cancelled",
    type(fval) == "table" and fval.code == "daemon", vim.inspect(fval))
  ok("p13: ...preserving the daemon's message",
    type(fval) == "table" and type(fval.message) == "string"
      and fval.message:find("attach refused by the daemon", 1, true) ~= nil,
    vim.inspect(fval and fval.message))
  ok("p13: ...and the underlying cause",
    type(fval) == "table" and type(fval.cause) == "table"
      and fval.cause.message == "attach refused by the daemon", vim.inspect(fval and fval.cause))
  ok("p13: and the command handed it back", got1 ~= nil and got1.id == 42, vim.inspect(got1))

  -- ── existing connections → picker offers them + a Create row ──
  session.reset_for_tests()
  local spaces = { { id = 1, name = "LM", connections = {
    { id = 9, name = "pg", engine = "postgres" },
  } } }
  local c2 = cx({ spaces = spaces })
  session.attach(c2, {})
  local conn_labels
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.select = function(items, o, cb)
    conn_labels = {}
    for _, it in ipairs(items) do conn_labels[#conn_labels + 1] = o.format_item(it) end
    return cb(items[1])  -- pick the existing connection
  end
  local got2
  commands.choose_connection(function(conn) got2 = conn end)
  restore()
  ok("p13: the picker lists connections plus a Create row",
    conn_labels ~= nil and #conn_labels == 2
    and conn_labels[2]:find("Create", 1, true) ~= nil, vim.inspect(conn_labels))
  ok("p13: picking an existing connection adopts it without creating",
    got2 ~= nil and got2.id == 9 and c2.created == nil, vim.inspect(got2))

  -- ── choosing Create from a non-empty list ──
  session.reset_for_tests()
  local c3 = cx({ spaces = spaces, new_id = 77 })
  session.attach(c3, {})
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  drive(
    { ["connection name"] = "warehouse", ["dsn"] = "mysql://a:b@h/w" },
    function(items, o)
      if #items == 3 then return "mysql" end       -- engine select
      return items[#items]                          -- the Create row
    end)
  local got3
  commands.choose_connection(function(conn) got3 = conn end)
  restore()
  ok("p13: the Create row leads to conn.create + attach",
    c3.created ~= nil and c3.created.engine == "mysql"
    and c3.attached ~= nil and c3.attached.conn_id == 77
    and got3 ~= nil and got3.id == 77, vim.inspect({ c3.created, c3.attached }))

  -- ── cancelling the DSN creates nothing ──
  session.reset_for_tests()
  local c4 = cx({ spaces = { { id = 5, name = "AutoDB", connections = {} } } })
  session.attach(c4, {})
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  drive(
    { ["connection name"] = "x", ["dsn"] = "" },   -- empty dsn cancels
    function(items) return "sqlite" end)
  local got4 = "sentinel"
  commands.choose_connection(function(conn) got4 = conn end)
  restore()
  ok("p13: an empty dsn cancels and creates nothing",
    c4.created == nil and c4.attached == nil and got4 == nil,
    vim.inspect({ c4.created, got4 }))

  session.reset_for_tests()
end)()

-- ─────────── [14] session.notes_dir — the server is the authority ───────────
print("\n[14] notes_dir — reported by the daemon, with a matching fallback")
;(function()
  local session = require("autodb.session")

  -- A client that reports notes_dir over hello wins.
  session.reset_for_tests()
  local c = {
    _token = "t", is_ready = function() return true end,
    token = function(self) return self._token end,
    hello = function() return { server = "autodb", notes_dir = "/srv/autodb/notes" } end,
  }
  session.attach(c, {})
  ok("p14: notes_dir comes from the daemon's hello",
    session.notes_dir() == "/srv/autodb/notes", session.notes_dir())

  -- No client / no field → the same default the server would compute.
  session.reset_for_tests()
  local xdg = vim.env.XDG_DATA_HOME
  vim.env.XDG_DATA_HOME = "/tmp/xdgdata"
  ok("p14: falls back to $XDG_DATA_HOME/autodb/notes",
    session.notes_dir() == "/tmp/xdgdata/autodb/notes", session.notes_dir())
  vim.env.XDG_DATA_HOME = xdg  -- restore

  session.reset_for_tests()
end)()

-- ─────────── [15] notes — the client-side store + <leader>Dn ───────────
print("\n[15] notes — create / list / delete / scaffold, and <leader>Dn")
;(function()
  local session = require("autodb.session")
  local commands = require("autodb.commands")
  local keys = require("autodb.keys")

  -- Point the notes root at a temp dir via the XDG fallback (no client).
  session.reset_for_tests()
  local xdg = vim.env.XDG_DATA_HOME
  local tmp = vim.fn.tempname()
  vim.env.XDG_DATA_HOME = tmp
  local notes = require("autodb.notes")

  -- name rules mirror the server's CleanName.
  ok("p15: a plain name gains .sql", ({ notes.clean_name("scratch") })[1] == "scratch.sql")
  ok("p15: a separator is rejected", ({ notes.clean_name("a/b") })[1] == nil)
  ok("p15: a leading dot is rejected", ({ notes.clean_name(".hidden") })[1] == nil)
  ok("p15: an existing .sql suffix is not doubled",
    ({ notes.clean_name("q.sql") })[1] == "q.sql")

  -- create → list → duplicate-refuse → delete.
  local p1, e1 = notes.create(7, "hello", "SELECT 1;\n")
  ok("p15: create writes a note", type(p1) == "string" and vim.fn.filereadable(p1) == 1, tostring(e1))
  ok("p15: the body landed",
    table.concat(vim.fn.readfile(p1), "\n"):find("SELECT 1", 1, true) ~= nil)
  local lst = notes.list(7)
  ok("p15: list shows it", #lst == 1 and lst[1].name == "hello.sql", vim.inspect(lst))
  local dup, derr = notes.create(7, "hello")
  ok("p15: a duplicate is refused", dup == nil and tostring(derr):find("already exists", 1, true))
  ok("p15: delete removes it", notes.delete(7, "hello") == true and vim.fn.filereadable(p1) == 0)

  -- scaffold: SELECT over the quoted identifier, collision-suffixed name.
  ok("p15: scaffold_sql quotes the FROM target",
    notes.scaffold_sql('"public"."songs"'):find('FROM "public"."songs"', 1, true) ~= nil)
  local sp1 = notes.scaffold(7, "songs", '"public"."songs"')
  local sp2 = notes.scaffold(7, "songs", '"public"."songs"')
  ok("p15: first scaffold is <table>.sql", sp1 and sp1:match("songs%.sql$") ~= nil, tostring(sp1))
  ok("p15: a second scaffold does not clobber", sp2 and sp2:match("songs%-2%.sql$") ~= nil, tostring(sp2))
  ok("p15: scaffold refuses without a quoted identifier",
    ({ notes.scaffold(7, "songs", nil) })[1] == nil)

  -- <leader>Dn create path over a live (stub) session + selected workspace.
  session.reset_for_tests()
  local c = {
    _token = "t", is_ready = function() return true end,
    token = function(self) return self._token end, hello = function() return nil end,
    authed = function(_, m, _p, cb) return cb(nil, { message = "unused " .. m }) end,
  }
  session.attach(c, {})
  session.select_workspace({ id = 9, name = "WS" })
  local opened
  local orig_open, orig_input, orig_uis =
    commands._open_note_file, vim.ui.input, vim.api.nvim_list_uis
  commands._open_note_file = function(path) opened = path end
  vim.api.nvim_list_uis = function() return { { chan = 1 } } end
  vim.ui.input = function(_, cb) cb("plan") end
  local got
  commands.choose_note(function(p) got = p end)
  vim.wait(500, function() return got ~= nil end, 5)
  commands._open_note_file, vim.ui.input, vim.api.nvim_list_uis =
    orig_open, orig_input, orig_uis
  -- The success value is a TABLE since ADR-0078 §3.6 (lector impl-r0
  -- MF4): a bare string cannot grow a field and gives a caller nothing
  -- to branch on. This assertion carried the pre-ADR shape.
  ok("p15: <leader>Dn creates a note in the active workspace",
    type(got) == "table" and type(got.path) == "string"
      and got.path:match("ws%-9/plan%.sql$") ~= nil, vim.inspect(got))
  ok("p15: and opens it in the editor", opened == (type(got) == "table" and got.path),
    tostring(opened))
  ok("p15: " .. tostring(keys.NOTE) .. " is the note key", keys.NOTE == keys.PREFIX .. "n")

  vim.fn.delete(tmp, "rf")
  vim.env.XDG_DATA_HOME = xdg
  session.reset_for_tests()
end)()

-- ─────── [16] run_sql unwraps the exec.run_script envelope ───────
print("\n[16] run_sql — a SELECT shows its rows, not '0 row(s) affected'")
;(function()
  local session = require("autodb.session")
  local commands = require("autodb.commands")
  local results = require("autodb.results")

  -- exec.run_script wraps the last result: { statements, result = {...} }.
  local ENVELOPE = {
    statements = 1,
    result = {
      verb = "select", class = "read",
      columns = { "id", "title" },
      rows = { { 1, "a" }, { 2, "b" } },
      affected = 0, more = false, duration_ms = 3,
    },
  }

  session.reset_for_tests()
  local c = {
    _token = "t", is_ready = function() return true end,
    token = function(self) return self._token end, hello = function() return nil end,
    authed = function(_, method, _p, cb)
      if method == "exec.run_script" then return cb(ENVELOPE, nil) end
      return cb(nil, { message = "unexpected " .. method })
    end,
  }
  session.attach(c, {})
  session.select_workspace({ id = 1, name = "WS" })
  session.select_connection({ id = 1, name = "pg" })

  -- Capture what results.show_result receives.
  local orig = results.show_result
  local seen_res, seen_err
  results.show_result = function(res, err) seen_res, seen_err = res, err end
  commands.run_sql("SELECT * FROM t")
  results.show_result = orig

  ok("p16: show_result got a result (not nil)", seen_res ~= nil and seen_err == nil,
    vim.inspect(seen_res))
  ok("p16: it is the INNER result — columns present",
    type(seen_res) == "table" and type(seen_res.columns) == "table"
    and #seen_res.columns == 2, vim.inspect(seen_res and seen_res.columns))
  ok("p16: the rows came through", type(seen_res) == "table"
    and type(seen_res.rows) == "table" and #seen_res.rows == 2,
    vim.inspect(seen_res and seen_res.rows))
  ok("p16: the envelope was unwrapped (no leftover .statements/.result)",
    type(seen_res) == "table" and seen_res.statements == nil and seen_res.result == nil)

  -- The PURE-DDL case: exec.run_script returns an envelope with NO `result` (the
  -- Go handler sets reply.result only when out.Last ~= nil, rpc/methods.go). The
  -- mock above always set it, so commands.lua's else-branch — "no result set" —
  -- was never exercised, directly adjacent to the '0 row(s) affected' bug section
  -- [16] exists to catch (mock-drift finding, lector r?/test-health).
  local ddl = { _token = "t", is_ready = function() return true end,
    token = function(self) return self._token end, hello = function() return nil end,
    authed = function(_, method, _p, cb)
      if method == "exec.run_script" then return cb({ statements = 2 }, nil) end
      return cb(nil, { message = "unexpected " .. method })
    end,
  }
  session.attach(ddl, {})
  session.select_workspace({ id = 1, name = "WS" })
  session.select_connection({ id = 1, name = "pg" })

  local log = require("autodb.log")
  local orig_show, orig_notify = results.show_result, log.notify
  local ddl_shown, ddl_notice = false, nil
  results.show_result = function() ddl_shown = true end
  log.notify = function(msg) ddl_notice = msg end
  commands.run_sql("CREATE TABLE t (id int)")
  results.show_result, log.notify = orig_show, orig_notify

  ok("p16: a no-result (pure DDL) envelope does NOT open the result panel",
    ddl_shown == false)
  ok("p16: it reports that the script ran with no result set",
    type(ddl_notice) == "string" and ddl_notice:find("no result set", 1, true) ~= nil,
    tostring(ddl_notice))

  session.reset_for_tests()
end)()


-- ─────── [17] ADR-0066 detail views + selection mode ───────
print("\n[17] detail views — a value, not '(no help entries)'")
;(function()
  local results = require("autodb.results")
  local NL = string.char(10)

  -- Invoke a key by looking its callback up in the BUFFER'S REAL KEYMAP.
  -- An unbound or misnamed key resolves to nil and fails the assertion,
  -- which is the point: calling the handler function directly would pass
  -- even if nothing were bound to it.
  local function press(buf, lhs)
    for _, m in ipairs(vim.api.nvim_buf_get_keymap(buf, "n")) do
      if m.lhs == lhs and m.callback then m.callback(); return true end
    end
    return false
  end

  results.reset_for_tests()
  local model = require("auto-core.ui.grid").model({
    columns = { "id", "body", "empty" },
    rows = { { 1, "line1" .. NL .. "line2" .. NL .. "line3", nil } },
  })
  local view = results.show(model)
  ok("p17: the result panel opened", view ~= nil)

  -- ── cell mode: <CR> goes STRAIGHT to the value ──
  vim.api.nvim_win_set_cursor(view:win(), { 1, 0 })
  view:inspect()
  local root, kind = results.detail_root()
  ok("p17: cell mode <CR> opens a cell view as the ROOT",
    root ~= nil and root:is_open() and kind == "cell", tostring(kind))
  local cl = root and vim.api.nvim_buf_get_lines(root:buf(), 0, -1, false) or {}
  ok("p17: it shows a value, not '(no help entries)'",
    not vim.tbl_contains(cl, "  (no help entries)"), vim.inspect(cl[1]))
  root:close()
  ok("p17: closing the root clears it", results.detail_root() == nil)

  -- ── the multi-line value renders its REAL newlines in the cell view ──
  vim.api.nvim_win_set_cursor(view:win(), { 1, 0 })
  view:move_cell(0, 1)  -- onto `body`
  view:inspect()
  local cv = results.detail_root()
  local body = vim.api.nvim_buf_get_lines(cv:buf(), 0, -1, false)
  ok("p17: a 3-line value occupies 3 lines in the CELL view", #body == 3, #body)
  ok("p17: y inside the cell view is bound", press(cv:buf(), "y"))
  ok("p17: and it yanked the faithful value with newlines intact",
    select(2, vim.fn.getreg('"'):gsub(NL, "")) == 2, vim.inspect(vim.fn.getreg('"')))

  -- ── row mode: <CR> opens the row, one line per column ──
  view:set_selection_mode("row")
  ok("p17: the view is in row mode", view:selection_mode() == "row")
  view:inspect()
  local rv, rkind = results.detail_root()
  ok("p17: row mode <CR> opens a row view as the ROOT",
    rv ~= nil and rv:is_open() and rkind == "row", tostring(rkind))
  local rl = vim.api.nvim_buf_get_lines(rv:buf(), 0, -1, false)
  ok("p17: THREE lines for three columns, despite the 3-line value",
    #rl == 3, #rl .. " -> " .. vim.inspect(rl))

  -- ── drill: <CR> on line 3 must reach column 3, not the value's tail ──
  vim.api.nvim_win_set_cursor(rv:win(), { 3, 0 })
  ok("p17: <CR> is bound in the row view", press(rv:buf(), "<CR>"))
  ok("p17: the row view is STILL the root — a child is not a root",
    select(1, results.detail_root()) == rv)
  local child_win = vim.api.nvim_get_current_win()
  local child_buf = vim.api.nvim_win_get_buf(child_win)
  ok("p17: drilling opened a child view", child_win ~= rv:win())
  ok("p17: and it is column 3 — the NULL one, so mapping did not drift",
    vim.api.nvim_buf_get_lines(child_buf, 0, -1, false)[1] == "NULL",
    vim.inspect(vim.api.nvim_buf_get_lines(child_buf, 0, -1, false)))
  ok("p17: y on a NULL yields the EMPTY string, not 'NULL'",
    press(child_buf, "y") and vim.fn.getreg('"') == "", vim.inspect(vim.fn.getreg('"')))

  -- ── CHILD RETURN (criterion 15): same window AND same cursor line ──
  vim.api.nvim_win_set_cursor(rv:win(), { 2, 0 })
  press(rv:buf(), "<CR>")
  local c2 = vim.api.nvim_get_current_win()
  vim.api.nvim_win_close(c2, true)
  ok("p17: CHILD RETURN — the row view is still open", rv:is_open())
  ok("p17: CHILD RETURN — focus is back on the row view",
    vim.api.nvim_get_current_win() == rv:win(),
    string.format("cur=%s row=%s", vim.api.nvim_get_current_win(), rv:win()))
  ok("p17: CHILD RETURN — on the SAME cursor line it was opened from",
    vim.api.nvim_win_get_cursor(rv:win())[1] == 2,
    vim.inspect(vim.api.nvim_win_get_cursor(rv:win())))

  -- ── at most ONE child: a second drill replaces the first ──
  press(rv:buf(), "<CR>")
  local first_child = vim.api.nvim_get_current_win()
  vim.api.nvim_set_current_win(rv:win())
  vim.api.nvim_win_set_cursor(rv:win(), { 1, 0 })
  press(rv:buf(), "<CR>")
  local second_child = vim.api.nvim_get_current_win()
  ok("p17: a second drill REPLACES the first child",
    second_child ~= first_child and not vim.api.nvim_win_is_valid(first_child),
    string.format("first=%s valid=%s second=%s", first_child,
      vim.api.nvim_win_is_valid(first_child), second_child))

  -- ── Y is absolute inside the detail views ──
  local sc_buf = vim.api.nvim_win_get_buf(second_child)
  ok("p17: Y is bound in the cell view", press(sc_buf, "Y"))
  ok("p17: and yanks the whole row as CSV",
    vim.fn.getreg('"'):find("line1", 1, true) ~= nil, vim.inspect(vim.fn.getreg('"')))

  -- ── PARENT CASCADE (criterion 16), via an EXTERNAL close ──
  vim.api.nvim_win_close(rv:win(), true)
  ok("p17: PARENT CASCADE — an external parent close leaves no orphan child",
    not vim.api.nvim_win_is_valid(second_child), tostring(second_child))
  ok("p17: and the root is cleared", results.detail_root() == nil)

  -- ── REGRESSION (impl-review MF2): a second root REPLACES the first ──
  -- Opening a row detail while one was already showing used to leave the
  -- first floating with nothing tracking it, and close() tore down only the
  -- newer one.
  local m2 = require("auto-core.ui.grid").model({
    columns = { "a" }, rows = { { "1" }, { "2" } },
  })
  local vv = results.show(m2)
  local root1 = results.open_row(m2, 1, vv:win())
  local root2 = results.open_row(m2, 2, vv:win())
  ok("p17: REGRESSION — opening a second root CLOSED the first",
    not root1:is_open() and root2:is_open(),
    string.format("first=%s second=%s", root1:is_open(), root2:is_open()))
  results.close()
  ok("p17: REGRESSION — close() leaves no root behind",
    not root2:is_open() and results.detail_root() == nil)

  -- ── STICKY MODE across a re-query (criterion 10) ──
  ok("p17: the sticky mode recorded the switch to row",
    results.selection_mode() == "row", results.selection_mode())
  local view2 = results.show(require("auto-core.ui.grid").model({
    columns = { "x" }, rows = { { "1" } },
  }))
  ok("p17: STICKY — a NEW query's grid is still in row mode",
    view2:selection_mode() == "row", view2:selection_mode())
  view2:set_selection_mode("cell")
  local view3 = results.show(require("auto-core.ui.grid").model({
    columns = { "x" }, rows = { { "1" } },
  }))
  ok("p17: STICKY — and switching back persists too",
    view3:selection_mode() == "cell", view3:selection_mode())

  results.close()
  results.reset_for_tests()
end)()

print("\n[18] the drawer — instances, host arbitration, and teardown (ADR-0078)")
;(function()
  local drawer = require("autodb.views.drawer")
  local hostreg = drawer._host_for_tests
  local panel_mod = require("auto-core").ui.panel

  local function reset()
    hostreg._reset_for_tests()
    require("autodb.panel")._reset_for_tests()
  end
  reset()

  -- ── the view contract (ADR-0078 §3.2) ───────────────────────
  local v = drawer.new()
  ok("p18: bufnr() is nil before the first get_buffer", v:bufnr() == nil)
  local b = v:get_buffer(vim.api.nvim_get_current_win())
  ok("p18: get_buffer returns a live scratch buffer",
    b and vim.api.nvim_buf_is_valid(b) and vim.bo[b].buftype == "nofile" and vim.bo[b].swapfile == false)
  ok("p18: bufnr() is that buffer once mounted", v:bufnr() == b)
  ok("p18: the default profile is autodb's own identity",
    vim.bo[b].filetype == "autodb" and vim.b[b].autodb_view == "drawer"
      and vim.api.nvim_buf_get_name(b):find("autodb://drawer", 1, true) ~= nil)

  -- Migrated from auto-finder smoke [49], which drove this renderer while it
  -- still lived there. The vocabulary is the view's, so it travels with it.
  local maps = {}
  for _, m in ipairs(vim.api.nvim_buf_get_keymap(b, "n")) do maps[m.lhs] = true end
  ok("p18: the panel vocabulary is bound (<CR>, o, i, R, ?)",
    maps["<CR>"] and maps["o"] and maps["i"] and maps["R"] and maps["?"],
    vim.inspect(vim.tbl_keys(maps)))
  ok("p18: h and l stay ordinary cursor motions", maps["h"] == nil and maps["l"] == nil)
  -- With no session the drawer states that and names the way forward,
  -- rather than rendering an empty pane. (auto-finder's [49] looked for
  -- "autodb is not installed" — that was ITS placeholder screen, a
  -- different surface that stays in auto-finder with the facade.)
  local rendered = table.concat(vim.api.nvim_buf_get_lines(b, 0, -1, false), "\n")
  ok("p18: with no session it says so instead of rendering empty",
    rendered:find("Not connected", 1, true) ~= nil, rendered)
  ok("p18: and points at the way forward", rendered:find("sign in", 1, true) ~= nil, rendered)
  ok("p18: rows stay parallel to lines",
    v._st._rows and #v._st._rows == #vim.api.nvim_buf_get_lines(b, 0, -1, false),
    tostring(v._st._rows and #v._st._rows))

  -- Criterion 7: subscriptions exist WITHOUT auto-finder, and go away.
  -- Before ADR-0078 this was the silent defect: the renderer reached into
  -- auto-finder.shared.view_subs behind a pcall, so standalone it rendered
  -- one frame and never refreshed again.
  ok("p18: subscriptions are live with no auto-finder present", v._sub_count() > 0,
    tostring(v._sub_count()))
  v:dispose()
  ok("p18: dispose releases the buffer and every handle",
    v:bufnr() == nil and v._sub_count() == 0)
  v:dispose()
  ok("p18: dispose is idempotent", v:bufnr() == nil)

  -- Criterion 5/9: instances share nothing, so one buffer is never mounted
  -- in two panels (auto-core restamps b:auto_core_panel_owner every mount).
  local a1, a2 = drawer.new(), drawer.new()
  local b1 = a1:get_buffer(vim.api.nvim_get_current_win())
  local b2 = a2:get_buffer(vim.api.nvim_get_current_win())
  ok("p18: two instances own two different buffers", b1 ~= b2)
  ok("p18: two instances share no state", a1._st ~= a2._st)
  a1:dispose(); a2:dispose()

  -- Criterion 6: the auto-finder profile reproduces that plugin's identity
  -- exactly. Its smoke asserts all three, so this is the regression gate.
  local af = drawer.new({ filetype = "auto-finder", buf_var = "auto_finder_view",
    buf_var_value = "dbase", buf_name = "auto-finder://dbase" })
  local bf = af:get_buffer(vim.api.nvim_get_current_win())
  ok("p18: the auto-finder profile yields ft/var/name unchanged",
    vim.bo[bf].filetype == "auto-finder" and vim.b[bf].auto_finder_view == "dbase"
      and vim.api.nvim_buf_get_name(bf):find("auto-finder://dbase", 1, true) ~= nil)
  af:dispose()

  -- ── host arbitration (ADR-0078 §3.3) ────────────────────────
  reset()
  local calls = { mount = 0, focus = 0, close = 0 }
  local last_release
  local function fake(id, priority, opts)
    opts = opts or {}
    return {
      id = id, priority = priority,
      profile = drawer.DEFAULT_PROFILE,
      available = function()
        if opts.available_raises then error("available boom") end
        return opts.available ~= false
      end,
      mount = function(view, release)
        calls.mount = calls.mount + 1
        last_release = release
        if opts.mount_raises then error("mount boom") end
        if opts.mount_nil then return nil end
        if opts.mount_invalid then return 999999 end
        local w = vim.api.nvim_get_current_win()
        if opts.mount_wrong_buffer then return w end   -- window shows someone else
        vim.api.nvim_win_set_buf(w, view:get_buffer(w))
        return w
      end,
      focus = function()
        calls.focus = calls.focus + 1
        if opts.focus_raises then error("focus boom") end
        return vim.api.nvim_get_current_win()
      end,
      close = function()
        calls.close = calls.close + 1
        if opts.close_raises then error("close boom") end
      end,
    }
  end

  ok("p18: register_host accepts a well-formed provider", drawer.register_host(fake("a", 10)) == true)
  local dup_ok, dup_err = drawer.register_host(fake("b", 10))
  ok("p18: a duplicate priority is REFUSED with a machine-readable code",
    dup_ok == false and dup_err and dup_err.code == "duplicate_priority",
    vim.inspect(dup_err))
  local bad_ok, bad_err = drawer.register_host({ id = "c", priority = 1 })
  ok("p18: a malformed provider is refused, not raised",
    bad_ok == false and bad_err and bad_err.code == "invalid")

  -- The self-host's identity is RESERVED, both halves. A foreign
  -- provider at priority 0 would displace the guaranteed standalone
  -- fallback and make autodb.panel.setup() fail, leaving a user with
  -- nowhere to put the drawer (lector impl-r0 MF2).
  local z_ok, z_err = drawer.register_host(fake("squatter", 0))
  ok("p18: priority 0 is reserved for autodb's self-host",
    z_ok == false and z_err and z_err.code == "duplicate_priority", vim.inspect(z_err))
  local i_ok, i_err = drawer.register_host(fake("autodb", 7))
  ok("p18: the id 'autodb' is reserved too",
    i_ok == false and i_err and i_err.code == "invalid", vim.inspect(i_err))
  local hj_ok, hj_err = drawer.register_host(fake("autodb", 0))
  ok("p18: a FABRICATED provider cannot claim the reserved id",
    hj_ok == false and hj_err and hj_err.code == "invalid", vim.inspect(hj_err))
  -- ...and the genuine one still gets in, through the internal path.
  -- Isolated: the fakes above outrank priority 0, so the fallback can
  -- only be observed as the winner when it is the only host left.
  reset()
  ok("p18: autodb's own panel still registers as the self-host",
    require("autodb.panel").setup() == true)
  ok("p18: ...and it answers when nothing outranks it",
    (function()
      local h; drawer.open(function(o, v) h = o and v.host or nil end); return h
    end)() == "autodb")
  reset()
  drawer.register_host(fake("a", 10))
  for _, bad in ipairs({ 0 / 0, math.huge, -math.huge, 1.5, "9" }) do
    local pok = drawer.register_host(fake("weird", bad))
    ok("p18: a non-integer priority (" .. tostring(bad) .. ") is refused", pok == false)
  end

  local oo, ov
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: open mounts on the available host", oo == true and ov.host == "a", vim.inspect(ov))
  ok("p18: owner() names it", drawer.owner() == "a")
  local mounted_view = drawer.mounted_view()
  ok("p18: the mounted view has a live buffer", mounted_view and mounted_view:bufnr() ~= nil)

  -- Criterion 8a: a HOST-initiated release ends central ownership, and the
  -- next open is a FRESH mount rather than a focus of the disposed surface.
  local dead = mounted_view
  last_release()
  ok("p18: release() clears the owner", drawer.owner() == nil)
  ok("p18: release() disposed the view", dead:bufnr() == nil and dead._sub_count() == 0)
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: reopening is a FRESH mount, not a focus of the dead surface",
    oo == true and drawer.mounted_view() ~= dead and drawer.mounted_view():bufnr() ~= nil)

  -- A release from a SUPERSEDED mount must not clear its successor.
  local stale_release = last_release
  drawer.register_host(fake("z", 50))            -- higher priority
  drawer.open(function() end)                    -- handoff: z takes over
  ok("p18: a higher-priority host wins the handoff", drawer.owner() == "z")
  local live = drawer.mounted_view()
  stale_release()
  ok("p18: a release from a superseded mount is a no-op",
    drawer.owner() == "z" and drawer.mounted_view() == live and live:bufnr() ~= nil)

  -- Criterion 8b/8c: provider failures leave no half-mounted world.
  reset()
  drawer.register_host(fake("raise", 10, { mount_raises = true }))
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: a raising mount reports host_failed and leaves NO owner",
    oo == false and ov.code == "host_failed" and drawer.owner() == nil, vim.inspect(ov))

  reset()
  drawer.register_host(fake("nilw", 10, { mount_nil = true }))
  drawer.open(function(o) oo = o end)
  ok("p18: a mount returning nil takes the same rollback", oo == false and drawer.owner() == nil)

  reset()
  drawer.register_host(fake("badw", 10, { mount_invalid = true }))
  drawer.open(function(o) oo = o end)
  ok("p18: a mount returning an invalid winid takes the rollback",
    oo == false and drawer.owner() == nil)

  reset()
  drawer.register_host(fake("wrongbuf", 10, { mount_wrong_buffer = true }))
  drawer.open(function(o) oo = o end)
  ok("p18: a window showing SOMEONE ELSE'S buffer is not a successful mount",
    oo == false and drawer.owner() == nil)

  reset()
  drawer.register_host(fake("noavail", 10, { available = false }))
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: an unavailable host is skipped -> no_host", oo == false and ov.code == "no_host")

  reset()
  drawer.register_host(fake("boom", 10, { available_raises = true }))
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: a RAISING available() is treated as unavailable, not fatal",
    oo == false and ov.code == "no_host")

  -- A host that cannot tidy up must still not strand the instance.
  reset()
  drawer.register_host(fake("dirty", 10, { close_raises = true }))
  drawer.open(function() end)
  local stranded = drawer.mounted_view()
  drawer.unregister_host("dirty")
  ok("p18: a raising close() still disposes and clears",
    drawer.owner() == nil and stranded:bufnr() == nil)

  -- ── the self-hosted panel (criteria 1 and 8) ────────────────
  reset()
  require("autodb.panel").setup()
  drawer.open(function(o, val) oo, ov = o, val end)
  ok("p18: with auto-core only, autodb self-hosts the drawer",
    oo == true and ov.host == "autodb", vim.inspect(ov))
  local pv = drawer.mounted_view()
  ok("p18: the drawer is visible in a real window",
    #vim.fn.win_findbuf(pv:bufnr()) > 0)
  local p = panel_mod.get("autodb")
  ok("p18: the panel is named autodb, never auto-finder", p ~= nil and panel_mod.get("auto-finder") == nil)
  p:close()
  vim.wait(50)
  ok("p18: closing the panel disposes the buffer, the handles and the owner",
    drawer.owner() == nil and pv:bufnr() == nil and pv._sub_count() == 0)
  drawer.open(function(o) oo = o end)
  ok("p18: and reopening mounts a fresh view", oo == true and drawer.mounted_view() ~= pv)
  reset()
end)()

print("\n[19] autodb.api — the public contract (ADR-0078 §3.6)")
;(function()
  local api = require("autodb.api")

  -- Table-driven, because the ADR's table IS the contract: every entry
  -- exists, is callable, and reports through a callback rather than
  -- raising into whatever bound it (lector impl-r0 MF4).
  local surface = {
    { "login", 1 }, { "choose_workspace", 1 }, { "choose_connection", 1 },
    { "choose_note", 1 }, { "history", 1 }, { "run_buffer", 1 },
    { "run_selection", 1 }, { "run_sql", 2 }, { "maintenance", 1 },
    { "drawer_open", 1 }, { "drawer_toggle", 1 }, { "drawer_focus", 1 },
    { "health", 0 },
  }
  for _, e in ipairs(surface) do
    ok("p19: api." .. e[1] .. " exists and is a function", type(api[e[1]]) == "function")
  end
  ok("p19: register_host is NOT on the api (host integration is on views.drawer)",
    api.register_host == nil and type(require("autodb.views.drawer").register_host) == "function")

  -- health() returns DATA, not nil. It rendered through vim.health and
  -- returned nothing, so a programmatic caller got no contract at all.
  local h = api.health()
  ok("p19: health() returns a table", type(h) == "table", type(h))
  if type(h) == "table" then
    ok("p19: health reports connection state as booleans",
      type(h.connected) == "boolean" and type(h.signed_in) == "boolean",
      vim.inspect({ h.connected, h.signed_in }))
  end

  -- Nothing that fails may go silent: with no daemon reachable, the
  -- callback must still be CALLED, with a structured error. Before the
  -- fix _connected logged and returned, and cb never fired at all.
  local called, got_ok, got_val = false, nil, nil
  api.run_sql("select 1", function(o, v) called, got_ok, got_val = true, o, v end)
  vim.wait(2000, function() return called end)
  ok("p19: run_sql COMPLETES its callback with no daemon", called, "callback never fired")
  ok("p19: ...and reports a structured error, not a bare nil",
    called and got_ok == false and type(got_val) == "table" and type(got_val.code) == "string",
    vim.inspect(got_val))
  ok("p19: ...and does NOT call a daemon failure 'cancelled'",
    called and got_val and got_val.code ~= "cancelled", vim.inspect(got_val))

  -- Every picker must COMPLETE too, not just run_sql: all three called
  -- _connected without an on_fail, so with an unreachable daemon none of
  -- them ever invoked its public callback (lector impl-r1 MF3).
  for _, name in ipairs({ "choose_workspace", "choose_connection", "choose_note" }) do
    local fired, calls, val = false, 0, nil
    api[name](function(o, v) fired, calls, val = true, calls + 1, v end)
    vim.wait(2000, function() return fired end)
    ok("p19: " .. name .. " COMPLETES its callback with no daemon", fired,
      "callback never fired")
    ok("p19: " .. name .. " reports a structured error", fired and type(val) == "table"
      and type(val.code) == "string", vim.inspect(val))
    ok("p19: " .. name .. " calls back exactly once", calls <= 1, tostring(calls))
  end

  -- An empty statement is the caller's fault, and says so.
  local icalled, ival = false, nil
  api.run_sql("", function(o, v) icalled, ival = true, v end)
  ok("p19: run_sql('') reports invalid", icalled and ival and ival.code == "invalid",
    vim.inspect(ival))
end)()

if #missing_prereqs > 0 then
  print(string.format("\n%d MISSING PRECONDITION(S): %s",
    #missing_prereqs, table.concat(missing_prereqs, ", ")))
end

-- The assertion floor: fewer assertions than expected means some vanished, which
-- is a failure in its own right (see EXPECTED_MIN_ASSERTIONS).
local total = pass_count + fail_count
local below_floor = total < EXPECTED_MIN_ASSERTIONS
if below_floor then
  print(string.format("\nASSERTION FLOOR: ran %d, expected at least %d — some "
    .. "assertions did not run", total, EXPECTED_MIN_ASSERTIONS))
end

print(string.format("\n%d passed, %d failed, %d missing (of >= %d expected)",
  pass_count, fail_count, #missing_prereqs, EXPECTED_MIN_ASSERTIONS))

-- SMOKE-COMPLETE is the sentinel the runner matches as an EXACT full line. It
-- prints ONLY here, at the natural end of the main chunk, so its absence means the
-- chunk aborted — nvim exits 0 on an uncaught Lua error, so the runner cannot trust
-- the exit code alone. The explicit os.exit below (not a trailing `qa!`) closes the
-- window where a deferred error could fire after the token and still exit 0.
if fail_count > 0 or #missing_prereqs > 0 or below_floor then
  print("SMOKE-COMPLETE FAIL")
  io.stdout:flush()
  os.exit(1)
else
  print("SMOKE-COMPLETE OK")
  io.stdout:flush()
  os.exit(0)
end
