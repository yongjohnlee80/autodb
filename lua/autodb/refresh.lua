---autodb refresh — fetch the latest branch, rebuild, relaunch.
---
---`<leader>DX` → "refresh autodb" [DECISION — Johno, 2026-08-18:
---*"Refresh is basically fetching the latest branch and relaunching the
---BE"*].
---
---Three steps, each of which can refuse: pull the plugin's own checkout
---fast-forward-only, rebuild through the project's Makefile, then ask
---the running daemon to stop so the next command starts the new binary.
---
---**It only applies to ONE install shape**, and says so rather than
---appearing to work. If the binary in use came from `PATH` — Mason, `go
---install`, a package manager — then updating this checkout changes
---nothing about the executable that actually runs, and the honest answer
---is to name the tool that does own it.
---
---Building goes through `make build`, not a hand-written `go build`. The
---Makefile already carries the `-buildvcs=false` the family's
---bare+worktree layout needs AND the `-ldflags` that stamp the version —
---and that version is exactly what the stale-backend comparison reads.
---A second build recipe here would drift from it and quietly produce
---binaries that report `dev` forever.
---@module 'autodb.refresh'

local keys = require("autodb.keys")
local lifecycle = require("autodb.lifecycle")
local log = require("autodb.log")
local session = require("autodb.session")

local M = {}

---_run executes one command in `cwd` and reports its output on failure.
---@param cmd string[]
---@param cwd string
---@param cb fun(ok: boolean, output: string[])
local function _run(cmd, cwd, cb)
  local out = {}
  local function collect(_, data)
    for _, l in ipairs(data or {}) do
      if l ~= "" then out[#out + 1] = l end
    end
  end
  local job = vim.fn.jobstart(cmd, {
    cwd = cwd,
    stdout_buffered = false,
    stderr_buffered = false,
    on_stdout = collect,
    on_stderr = collect,
    on_exit = function(_, code)
      vim.schedule(function() cb(code == 0, out) end)
    end,
  })
  if job <= 0 then
    return cb(false, { "cannot start: " .. table.concat(cmd, " ") })
  end
end

---preflight decides whether a refresh can work at all.
---
---Refusing early and specifically is the whole value here: a refresh
---that pulls, builds, restarts and changes nothing the user can observe
---is worse than one that declines and explains.
---@param opts { bin: string?, config: string? }?
---@return boolean ok, string? err, table? plan
function M.preflight(opts)
  opts = opts or {}
  local bin, berr, label = lifecycle.resolve_binary(opts.bin)
  if not bin then return false, berr end

  local root = lifecycle.plugin_root()
  local plugin_bin = root .. "/bin/" .. lifecycle.BINARY_NAME

  if bin ~= plugin_bin then
    return false, string.format(
      "refresh updates this plugin's own checkout and rebuilds %s, but the "
      .. "binary in use is %s (%s).\n\n"
      .. "Update it with the tool that installed it instead:\n"
      .. "  Mason        :Mason, then update autodb\n"
      .. "  go install   go install github.com/yongjohnlee80/autodb/cmd/autodb@latest\n"
      .. "  package mgr  your usual upgrade command\n\n"
      .. "Then press %s and choose \"restart the backend\".",
      plugin_bin, bin, label, keys.MAINTENANCE)
  end

  if vim.fn.isdirectory(root .. "/.git") == 0 and vim.fn.filereadable(root .. "/.git") == 0 then
    return false, string.format(
      "%s is not a git checkout, so there is no branch to fetch.\n"
      .. "Reinstall the plugin, or build the binary yourself:\n"
      .. "  cd %s && make build", root, root)
  end

  return true, nil, { bin = bin, root = root }
end

---run performs the refresh.
---@param opts { bin: string?, config: string?, on_done: fun(ok: boolean, err: string?)? }?
function M.run(opts)
  opts = opts or {}
  local done = opts.on_done or function() end

  local ok, err, plan = M.preflight(opts)
  if not ok then
    log.notify(tostring(err), { level = "warn", component = "refresh" })
    return done(false, err)
  end

  local root = plan.root

  local function fail(step, output)
    local msg = string.format("refresh failed at %s:\n  %s\n\nYou can finish by hand:\n"
      .. "  cd %s && git pull --ff-only && make build", step,
      table.concat(output or {}, "\n  "), root)
    log.notify(msg, { level = "error", component = "refresh" })
    done(false, msg)
  end

  -- 1. Is the tree clean? A fast-forward pull would refuse anyway, but
  --    saying WHY beats relaying git's exit code.
  _run({ "git", "status", "--porcelain" }, root, function(sok, sout)
    if not sok then return fail("git status", sout) end
    if #sout > 0 then
      local msg = string.format(
        "%s has uncommitted changes, so refresh will not pull over them:\n  %s\n\n"
        .. "Commit or stash them, then try again.", root,
        table.concat(sout, "\n  "))
      log.notify(msg, { level = "warn", component = "refresh" })
      return done(false, msg)
    end

    log.notify("refresh: fetching…", { component = "refresh" })
    -- 2. Fast-forward ONLY. A refresh must never create a merge commit
    --    or rewrite anything in the user's checkout.
    _run({ "git", "pull", "--ff-only" }, root, function(pok, pout)
      if not pok then return fail("git pull --ff-only", pout) end
      local pulled = table.concat(pout, " ")
      local already = pulled:find("Already up to date", 1, true) ~= nil

      log.notify(already and "refresh: already current; rebuilding…"
        or "refresh: pulled; rebuilding…", { component = "refresh" })

      -- 3. Build through the Makefile, which owns the flags and the
      --    version stamp the stale-backend check reads.
      local build = vim.fn.executable("make") == 1 and { "make", "build" }
        or { "go", "build", "-o", "bin/" .. lifecycle.BINARY_NAME, "./cmd/autodb" }
      _run(build, root, function(bok, bout)
        if not bok then return fail(table.concat(build, " "), bout) end

        local version = lifecycle.binary_version(plan.bin) or "?"
        -- 4. Relaunch: ask the daemon to stop. It decides and drains
        --    (§3.7.3); the next command starts the new binary.
        if not session.is_ready() then
          log.notify("refresh: built " .. version
            .. " — no daemon running, so the next command starts it",
            { component = "refresh" })
          return done(true, nil)
        end
        session.authed("sys.shutdown", {}, function(_, serr)
          if serr then
            local msg = "refresh: built " .. version
              .. " but the backend refused to restart: " .. tostring(serr.message)
              .. "\nPress " .. keys.MAINTENANCE .. " and choose restart once you are admin."
            log.notify(msg, { level = "warn", component = "refresh" })
            return done(false, msg)
          end
          session.detach("refresh")
          log.notify("refresh: built " .. version
            .. " and asked the backend to restart; the next command uses it",
            { component = "refresh" })
          done(true, nil)
        end)
      end)
    end)
  end)
end

return M
