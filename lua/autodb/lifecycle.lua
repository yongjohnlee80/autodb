---autodb lifecycle — spawning, proving and recovering the daemon.
---
---**Read §3.7.0 of ADR-0058 before adding anything here.** This surface
---exists to save a developer some typing. The escape hatch is always
---present and always understood: clone the repo, build the binary, run
---`autodb --serve` in a terminal. Nothing here is the only way to get a
---working daemon.
---
---So the rule is: automate the happy path completely, resolve the states
---we can identify with confidence, and for anything else **stop and say
---exactly what was found**, naming the pid, the address and the file, so
---the developer can finish by hand. A recovery path intricate enough to
---need its own recovery path has crossed the line.
---
---Never signal a process we have not positively identified. Refusing is
---always available and always safe.
---@module 'autodb.lifecycle'

local M = {}

local STATE_SUBDIR = "autodb"
local LAUNCH_SUBDIR = "launch"
local RECORD_NAME = "spawn.json"
local LOCK_NAME = "lifecycle.lock"

-- How long a launch may take before takeover treats it as failed. Stored
-- ABSOLUTELY in the record: a relative wait would let each successive
-- takeover restart the clock, so a wedged launch would never resolve.
local SPAWN_DEADLINE_MS = 15000

---state_dir is where the record, lock and nonces live: 0700, because
---the record is part of the launch proof and the nonce is a credential.
---@return string
function M.state_dir()
  local dir = vim.fn.stdpath("state") .. "/" .. STATE_SUBDIR
  if vim.fn.isdirectory(dir) == 0 then
    vim.fn.mkdir(dir, "p", tonumber("700", 8))
  end
  return dir
end

function M.record_path() return M.state_dir() .. "/" .. RECORD_NAME end
function M.lock_path() return M.state_dir() .. "/" .. LOCK_NAME end

function M.launch_dir()
  local dir = M.state_dir() .. "/" .. LAUNCH_SUBDIR
  if vim.fn.isdirectory(dir) == 0 then
    vim.fn.mkdir(dir, "p", tonumber("700", 8))
  end
  return dir
end

---new_nonce returns 32 CSPRNG bytes, hex encoded.
---
---`math.random` is not acceptable here: it is seeded predictably and a
---guessable launch proof proves nothing. `/dev/urandom` is the source;
---if it cannot be read we fail rather than fall back to something
---weaker, because a weak nonce is worse than an honest error.
---@return string|nil nonce, string? err
function M.new_nonce()
  local fh = io.open("/dev/urandom", "rb")
  if not fh then
    return nil, "autodb: cannot open /dev/urandom for a launch nonce"
  end
  local raw = fh:read(32)
  fh:close()
  if not raw or #raw < 32 then
    return nil, "autodb: short read from /dev/urandom"
  end
  return (raw:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

---write_atomic writes via temp + rename so no reader ever sees a
---half-written file. `mode` is applied to the temp file BEFORE the
---rename, so the final name is never briefly world-readable.
---@param path string
---@param lines string[]
---@param mode string?
---@return boolean ok, string? err
function M.write_atomic(path, lines, mode)
  local tmp = path .. ".tmp"
  if vim.fn.writefile(lines, tmp) ~= 0 then
    return false, "autodb: cannot write " .. tmp
  end
  if mode then vim.fn.setfperm(tmp, mode) end
  local ok = os.rename(tmp, path)
  if not ok then
    vim.fn.delete(tmp)
    return false, "autodb: cannot rename " .. tmp .. " to " .. path
  end
  return true
end

---read_record loads the spawn record, or nil when there is none.
---
---A record that is present but unparseable is NOT treated as absent:
---that is precisely the "cannot identify with confidence" case, and
---silently overwriting it would destroy the evidence a developer needs.
---@return table|nil record, string? err
function M.read_record()
  local path = M.record_path()
  if vim.fn.filereadable(path) == 0 then return nil end
  local lines = vim.fn.readfile(path)
  local okj, decoded = pcall(vim.json.decode, table.concat(lines, "\n"))
  if not okj or type(decoded) ~= "table" then
    return nil, string.format(
      "autodb: the spawn record at %s is unreadable. Inspect or remove it, " ..
      "or run `autodb --serve` yourself.", path)
  end
  return decoded
end

---write_record persists the record atomically at 0600.
---@param rec table
---@return boolean ok, string? err
function M.write_record(rec)
  return M.write_atomic(M.record_path(), { vim.json.encode(rec) }, "rw-------")
end

---clear_record tombstones a resolved generation.
---
---A tombstone rather than a delete, so a later takeover can tell "this
---generation was resolved" from "there has never been a record".
---@param generation string
function M.tombstone(generation)
  M.write_record({
    state = "tombstone",
    generation = generation,
    cleared_at = os.time(),
  })
end

---pid_alive reports whether a pid exists at all.
---@param pid integer
---@return boolean
function M.pid_alive(pid)
  if type(pid) ~= "number" or pid <= 0 then return false end
  return vim.fn.isdirectory("/proc/" .. tostring(pid)) == 1
end

---pid_identity returns the start time and executable for a pid, which
---together with the pid form an identity that survives pid reuse.
---
---A pid alone is not an identity. Pids are recycled, and signalling a
---recycled pid kills an unrelated process — which is why every caller
---here compares all three before it signals anything.
---@param pid integer
---@return { start: string, exe: string }|nil
function M.pid_identity(pid)
  if not M.pid_alive(pid) then return nil end
  local stat = vim.fn.readfile("/proc/" .. tostring(pid) .. "/stat")[1]
  if not stat then return nil end
  -- Field 22 is starttime, but the comm field may contain spaces inside
  -- parentheses, so split from the closing paren rather than the start.
  local after = stat:match("%)%s+(.*)$")
  if not after then return nil end
  local fields = vim.split(after, " ", { plain = true })
  local start = fields[20] -- state is field 3 overall; 20 here == 22 overall
  local exe = vim.fn.resolve("/proc/" .. tostring(pid) .. "/exe")
  return { start = tostring(start), exe = exe }
end

---is_our_child reports whether `rec`'s recorded child is still the same
---process — pid AND start time AND executable.
---@param rec table
---@return boolean
function M.is_our_child(rec)
  if type(rec) ~= "table" or not rec.child_pid then return false end
  local ident = M.pid_identity(rec.child_pid)
  if not ident then return false end
  if rec.child_start and tostring(rec.child_start) ~= ident.start then return false end
  if rec.exe and rec.exe ~= "" and rec.exe ~= ident.exe then return false end
  return true
end

---@class AutodbLaunch
---@field generation string
---@field nonce string
---@field nonce_digest string
---@field nonce_path string
---@field addr string
---@field exe string

---prepare_launch creates the generation and its nonce file, and
---publishes the `starting` intent BEFORE anything is spawned.
---
---Publishing first is what makes a crash recoverable: a launcher killed
---between spawn and finalization would otherwise leave a live daemon
---with no durable record of what it was supposed to be.
---@param opts { addr: string, exe: string }
---@return AutodbLaunch|nil, string? err
function M.prepare_launch(opts)
  local nonce, nerr = M.new_nonce()
  if not nonce then return nil, nerr end

  local generation = tostring(os.time()) .. "-" .. nonce:sub(1, 8)
  local nonce_path = M.launch_dir() .. "/" .. generation .. ".nonce"

  -- The nonce file is 0600 and written before the spawn. A pre-existing
  -- path here means a colliding generation, which we refuse rather than
  -- overwrite: overwriting could hand our nonce to somebody else's
  -- pending launch.
  if vim.fn.filereadable(nonce_path) == 1 then
    return nil, "autodb: launch nonce " .. nonce_path .. " already exists"
  end
  if vim.fn.writefile({ nonce }, nonce_path) ~= 0 then
    return nil, "autodb: cannot write launch nonce " .. nonce_path
  end
  vim.fn.setfperm(nonce_path, "rw-------")

  local digest = vim.fn.sha256(nonce)
  local okw, werr = M.write_record({
    state = "starting",
    generation = generation,
    nonce_path = nonce_path,
    nonce_digest = digest,       -- the DIGEST, not the nonce: a verifier
    exe = opts.exe,              -- that outlives the file without being
    address = opts.addr,         -- a second copy of the secret
    launcher_pid = vim.fn.getpid(),
    created_at = os.time(),
    deadline = vim.uv.now() + SPAWN_DEADLINE_MS,
  })
  if not okw then
    vim.fn.delete(nonce_path)
    return nil, werr
  end

  return {
    generation = generation,
    nonce = nonce,
    nonce_digest = digest,
    nonce_path = nonce_path,
    addr = opts.addr,
    exe = opts.exe,
  }
end

---record_child stamps the child's pid and start time the moment the
---spawn returns — before waiting for anything, so a crash during the
---wait still leaves something identifiable.
---@param launch AutodbLaunch
---@param pid integer
function M.record_child(launch, pid)
  local rec = M.read_record() or {}
  if rec.generation ~= launch.generation then return end
  rec.child_pid = pid
  local ident = M.pid_identity(pid)
  rec.child_start = ident and ident.start or nil
  M.write_record(rec)
end

---finalize marks the generation `running` once hello has proved it.
---@param launch AutodbLaunch
---@param hello table
function M.finalize(launch, hello)
  M.write_record({
    state = "running",
    generation = launch.generation,
    nonce_digest = launch.nonce_digest,
    child_pid = hello.pid,
    child_start = (M.pid_identity(hello.pid) or {}).start,
    exe = launch.exe,
    address = hello.addr or launch.addr,
    instance = hello.instance,
  })
end

---abandon_launch cleans up a launch that never proved itself.
---@param launch AutodbLaunch
function M.abandon_launch(launch)
  vim.fn.delete(launch.nonce_path)
  M.tombstone(launch.generation)
end

---resolve_stale examines a `starting` record left by a launcher that is
---gone, and reports what should happen — WITHOUT doing it.
---
---Split from the action deliberately: deciding is testable, signalling
---is not, and the decision is the part that must never guess.
---@param rec table
---@param now integer  -- vim.uv.now()
---@return string action, string detail
---   "adopt"   — probe it; a matching nonce means it is healthy
---   "wait"    — inside the deadline; leave it alone for now
---   "clean"   — nothing of ours is running; unlink and tombstone
---   "kill"    — our child, positively identified, past the deadline
---   "refuse"  — cannot identify with confidence; report and stop
function M.resolve_stale(rec, now)
  if type(rec) ~= "table" then
    return "refuse", "the spawn record is not a table"
  end
  if rec.state == "running" then
    return "adopt", "a finalized record; probe it before doing anything"
  end
  if rec.state == "tombstone" then
    return "clean", "generation " .. tostring(rec.generation) .. " was already resolved"
  end
  if rec.state ~= "starting" then
    return "refuse", "unknown record state " .. vim.inspect(rec.state)
  end

  -- The deadline is checked FIRST, and nothing is touched before it
  -- passes. A launcher that has published its intent but not yet
  -- spawned is INSIDE its deadline and perfectly healthy; treating a
  -- missing child_pid as "nothing was launched" before then would let
  -- one Neovim instance wipe another's launch mid-flight — the exact
  -- concurrent-launcher hazard the record exists to prevent.
  if type(rec.deadline) == "number" and now < rec.deadline then
    return "wait", "the launch deadline has not passed"
  end

  if not rec.child_pid then
    -- Past the deadline with no child: the launcher died between
    -- publishing the intent and spawning, so there is nothing to signal.
    return "clean", "no child was ever spawned for generation " .. tostring(rec.generation)
  end

  if not M.pid_alive(rec.child_pid) then
    return "clean", "child " .. tostring(rec.child_pid) .. " is already gone"
  end
  if not M.is_our_child(rec) then
    -- The pid exists but is somebody else's now. Signalling it would
    -- kill an unrelated process.
    return "clean", string.format(
      "pid %s no longer matches the recorded start time and executable — " ..
      "it is not our child, so it is left alone", tostring(rec.child_pid))
  end
  return "kill", string.format(
    "child %s is ours and passed its launch deadline", tostring(rec.child_pid))
end

---describe_manual is what we print whenever we refuse.
---
---An honest, specific error beats a clever repair of a state we do not
---understand — the developer's fallback is one command (§3.7.0).
---@param reason string
---@param rec table|nil
---@return string
function M.describe_manual(reason, rec)
  local lines = { "autodb: " .. reason, "" }
  if type(rec) == "table" then
    lines[#lines + 1] = "  record:     " .. M.record_path()
    lines[#lines + 1] = "  generation: " .. tostring(rec.generation)
    lines[#lines + 1] = "  state:      " .. tostring(rec.state)
    lines[#lines + 1] = "  child pid:  " .. tostring(rec.child_pid)
    lines[#lines + 1] = "  address:    " .. tostring(rec.address)
    lines[#lines + 1] = ""
  end
  lines[#lines + 1] = "You can always run the server yourself:"
  lines[#lines + 1] = "  autodb --serve"
  return table.concat(lines, "\n")
end

return M
