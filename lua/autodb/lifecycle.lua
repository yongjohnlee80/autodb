---autodb lifecycle — find the daemon, or start one.
---
---**Read §3.7.0 of ADR-0058 before adding anything here.** This exists
---to save a developer typing. The escape hatch is always available and
---always understood: clone the repo, build the binary, run
---`autodb --serve` in a terminal. Nothing here is the only way to get a
---working daemon, so it automates the happy path, and for anything it
---cannot resolve with confidence it STOPS and says what it found.
---
---Deliberately small. An earlier revision carried a launch nonce, a
---generation id, a durable spawn record and an idempotent takeover state
---machine — all of it to answer "is the thing on that port really mine?"
---That question disappeared with the transport: the daemon listens on a
---unix socket at mode 0600 in a per-user directory, so no other user can
---open it and no other machine can reach it. Identity comes from the
---filesystem, exactly as it does for Neovim's own RPC socket, which is
---why every agent talking to `$NVIM` needs no handshake either.
---
---What remains is the part that was always the point: if nothing is
---listening, start something.
---@module 'autodb.lifecycle'

local M = {}

local SPAWN_TIMEOUT_MS = 10000
local PROBE_INTERVAL_MS = 100

---BINARY_NAME is what we look for on PATH.
M.BINARY_NAME = "autodb"

---plugin_root is this plugin's own directory, whatever installed it.
---
---Derived from this file's path rather than from the runtimepath, so it
---is correct under lazy, packer, a git clone, or a worktree — none of
---which agree on where a plugin lives.
---@return string
function M.plugin_root()
  local src = debug.getinfo(1, "S").source:sub(2)
  -- <root>/lua/autodb/lifecycle.lua -> <root>
  return vim.fn.fnamemodify(vim.fn.fnamemodify(src, ":p"), ":h:h:h")
end

---binary_candidates lists every place the executable might be, in
---priority order, with a label for each so a failure can say what was
---searched rather than just "not found".
---@param configured string?
---@return { label: string, path: string }[]
function M.binary_candidates(configured)
  local out = {}
  if configured and configured ~= "" then
    out[#out + 1] = { label = "configured (opts.bin)", path = vim.fn.expand(configured) }
  end
  -- PATH, which is how gopls and delve are found: Mason installs to its
  -- own bin directory and prepends that to PATH inside Neovim, so a
  -- Mason-managed autodb resolves here with no configuration at all.
  -- `go install` (~/go/bin), Homebrew and system packages land here too.
  --
  -- The bare NAME is the candidate, because vim.fn.executable() performs
  -- the PATH lookup itself. Listing it unconditionally means a failure
  -- can report that PATH was searched and came up empty, rather than
  -- leaving the reader to wonder whether it was consulted at all.
  out[#out + 1] = {
    label = "PATH (Mason, go install, package manager)",
    path = M.BINARY_NAME,
  }
  -- A plugin-local build, which is what a lazy.nvim `build` hook
  -- produces — the same shape nvim-dbee uses. Version-matched to this
  -- checkout by construction.
  out[#out + 1] = {
    label = "plugin build (lazy `build` hook)",
    path = M.plugin_root() .. "/bin/" .. M.BINARY_NAME,
  }
  -- A managed cache, for a binary this plugin downloaded itself.
  out[#out + 1] = {
    label = "managed cache",
    path = vim.fn.stdpath("data") .. "/autodb/bin/" .. M.BINARY_NAME,
  }
  return out
end

---resolve_binary finds the executable, or explains what it looked at.
---
---No guessing and no silent fallback: if nothing is found, the caller
---gets every path that was tried, because "autodb not found" without the
---search list is the least useful error a plugin can produce.
---@param configured string?
---@return string|nil path, string? err, string? label
function M.resolve_binary(configured)
  -- An explicit opts.bin is honoured or refused — never quietly
  -- replaced. Falling through to some other autodb would run a
  -- different build than the one the user named, which is precisely the
  -- surprise that produces "why is it still doing the old thing".
  if configured and configured ~= "" then
    local explicit = vim.fn.expand(configured)
    if vim.fn.executable(explicit) == 1 then
      return explicit, nil, "configured (opts.bin)"
    end
    return nil, M.describe_manual(
      string.format("opts.bin is set to %q, which is not executable. "
        .. "Fix the path or unset it to search PATH and the plugin build.", explicit))
  end

  local tried = {}
  for _, c in ipairs(M.binary_candidates(nil)) do
    if vim.fn.executable(c.path) == 1 then
      -- Report the RESOLVED location, so health output names a file
      -- rather than echoing back the word "autodb".
      local resolved = c.path
      if resolved == M.BINARY_NAME then resolved = vim.fn.exepath(M.BINARY_NAME) end
      return resolved, nil, c.label
    end
    tried[#tried + 1] = string.format("%-42s %s", c.label, c.path)
  end
  return nil, M.describe_manual(
    "no autodb executable found. Install it with Mason, `go install "
    .. "github.com/yongjohnlee80/autodb/cmd/autodb@latest`, or a lazy.nvim "
    .. "`build` hook — or set opts.bin to a path.", tried)
end

---binary_version reports what `autodb --version` prints.
---
---Used to answer "is the running daemon this build?" — a shared daemon
---outlives the frontend that started it BY DESIGN, so after a rebuild
---the old process keeps serving happily. That cost me two debugging
---sessions in M6, and the fix is to make it visible rather than to
---remember.
---@param bin string
---@return string|nil version, string? err
function M.binary_version(bin)
  if vim.fn.executable(bin) ~= 1 then
    return nil, string.format("autodb: %s is not executable", tostring(bin))
  end
  local out = vim.fn.systemlist({ bin, "--version" })
  if vim.v.shell_error ~= 0 then
    return nil, "autodb: --version failed: " .. table.concat(out or {}, " ")
  end
  -- "autodb <version> (<commit>, built <date>)"
  local line = (out or {})[1] or ""
  return line:match("^autodb%s+(%S+)") or line
end

---build_status compares the RUNNING daemon against the binary on disk.
---
---Returns one of "match", "stale", "unknown" plus a line for display.
---The frontend cannot fix a mismatch — restarting is the admin's call —
---so it does the one useful thing and names it.
---@param backend_version string?  from sys.hello
---@param bin string?
---@return string status, string message
function M.build_status(backend_version, bin)
  if not backend_version or backend_version == "" then
    return "unknown", "backend build unknown (not connected)"
  end
  local disk = bin and M.binary_version(bin) or nil
  if not disk then
    return "unknown", "backend " .. backend_version .. " (no local binary to compare)"
  end
  if disk == backend_version then
    return "match", backend_version .. " (matches the installed binary)"
  end
  return "stale", string.format(
    "%s \226\137\160 %s \226\128\148 the running server is a DIFFERENT build. "
    .. "Restart it to pick up the installed one.", backend_version, disk)
end

---resolve_endpoint asks the BINARY where to dial.
---
---The socket path depends on the platform (`XDG_RUNTIME_DIR` on Linux,
---`$TMPDIR` on macOS) and on the user's config. Reimplementing those
---rules here would be a second resolver that drifts from the Go one the
---moment either changes, so the binary reports its own answer
---([[shared-resolver-single-source-of-truth]]).
---@param bin string        -- path to the autodb executable
---@param config_path string?
---@return { mode: string, addr: string }|nil, string? err
function M.resolve_endpoint(bin, config_path)
  if vim.fn.executable(bin) ~= 1 then
    return nil, string.format("autodb: %s is not executable", tostring(bin))
  end
  local cmd = { bin, "--print-endpoint" }
  if config_path and config_path ~= "" then
    cmd[#cmd + 1] = "--config"
    cmd[#cmd + 1] = config_path
  end
  local out = vim.fn.systemlist(cmd)
  if vim.v.shell_error ~= 0 then
    return nil, string.format("autodb: --print-endpoint failed: %s",
      table.concat(out or {}, " "))
  end
  local line = (out or {})[1] or ""
  local network, addr = line:match("^(%S+)\t(.+)$")
  if not network or not addr then
    return nil, string.format("autodb: cannot parse endpoint %q", line)
  end
  -- Neovim calls a unix socket "pipe"; the wire name is "unix".
  return { mode = network == "unix" and "pipe" or "tcp", addr = addr }
end

---is_listening reports whether something answers at the endpoint.
---
---A connect attempt, not a file check: a socket FILE can outlive the
---process that made it (a SIGKILLed daemon leaves one behind), so its
---presence proves nothing. Only a successful connection does.
---@param ep { mode: string, addr: string }
---@return boolean
function M.is_listening(ep)
  local ok, chan = pcall(vim.fn.sockconnect, ep.mode, ep.addr, { rpc = false })
  if not ok or not chan or chan == 0 then return false end
  pcall(vim.fn.chanclose, chan)
  return true
end

---spawn starts a detached `autodb --serve` and waits for it to answer.
---
---The daemon is expected to outlive this Neovim: it is shared with the
---TUI and with other editors, so it is NOT a child we supervise. We
---start it and then treat it exactly like one we found already running.
---
---Racing launchers are safe without coordination here, because the
---server itself resolves the race: two spawns both try to bind, one
---wins, and the loser probes the winner, prints "already running" and
---exits 0. That guard lives in Go, next to the bind that needs it.
---@param opts { bin: string, config_path: string?, endpoint: table? }
---@param cb fun(ok: boolean, err: string|nil)
function M.spawn(opts, cb)
  local ep = opts.endpoint
  if not ep then
    local resolved, rerr = M.resolve_endpoint(opts.bin, opts.config_path)
    if not resolved then return cb(false, rerr) end
    ep = resolved
  end

  if M.is_listening(ep) then return cb(true, nil) end

  local cmd = { opts.bin, "--serve" }
  if opts.config_path and opts.config_path ~= "" then
    cmd[#cmd + 1] = "--config"
    cmd[#cmd + 1] = opts.config_path
  end

  local stderr = {}
  local job = vim.fn.jobstart(cmd, {
    detach = true,          -- it outlives this editor by design
    stderr_buffered = false,
    on_stderr = function(_, data)
      for _, l in ipairs(data or {}) do
        if l ~= "" then stderr[#stderr + 1] = l end
      end
    end,
  })
  if job <= 0 then
    return cb(false, string.format("autodb: cannot start %s", opts.bin))
  end

  -- Poll for the listener rather than assuming the spawn worked: bind
  -- happens after process start, and a startup failure (bad config,
  -- unreadable meta store) shows up here rather than as a silent hang.
  local waited = 0
  local timer = vim.uv.new_timer()
  timer:start(PROBE_INTERVAL_MS, PROBE_INTERVAL_MS, vim.schedule_wrap(function()
    waited = waited + PROBE_INTERVAL_MS
    if M.is_listening(ep) then
      timer:stop(); timer:close()
      return cb(true, nil)
    end
    if waited >= SPAWN_TIMEOUT_MS then
      timer:stop(); timer:close()
      return cb(false, M.describe_manual(string.format(
        "started %s but nothing answered on %s within %dms", opts.bin, ep.addr,
        SPAWN_TIMEOUT_MS), stderr))
    end
  end))
end

---ensure returns a live endpoint, starting a daemon if needed.
---@param opts { bin: string, config_path: string? }
---@param cb fun(endpoint: table|nil, err: string|nil)
function M.ensure(opts, cb)
  local ep, rerr = M.resolve_endpoint(opts.bin, opts.config_path)
  if not ep then return cb(nil, rerr) end
  if M.is_listening(ep) then return cb(ep, nil) end
  M.spawn({ bin = opts.bin, config_path = opts.config_path, endpoint = ep },
    function(ok, serr)
      if not ok then return cb(nil, serr) end
      cb(ep, nil)
    end)
end

---describe_manual is what we print when we give up.
---
---An honest, specific error beats a clever repair of a state we do not
---understand — the developer's fallback is one command (§3.7.0).
---@param reason string
---@param detail string[]?
---@return string
function M.describe_manual(reason, detail)
  local lines = { "autodb: " .. reason, "" }
  for _, l in ipairs(detail or {}) do
    lines[#lines + 1] = "  " .. l
  end
  if #(detail or {}) > 0 then lines[#lines + 1] = "" end
  lines[#lines + 1] = "You can always run the server yourself:"
  lines[#lines + 1] = "  autodb --serve"
  return table.concat(lines, "\n")
end

return M
