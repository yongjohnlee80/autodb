---@module 'autodb.txstatus'
---
---The transaction-outcome poll surface (`tx.status`, protocol 5).
---
---Deliberately NOT built on the history list. History disappears entirely
---when `[history].enabled = false`, and a boundary-only `BEGIN; COMMIT;`
---never had a history row at all -- so a projection over it could not answer
---for the two cases that most need answering. This asks the server's own
---outcome API, which is the single place that folds the transition log.

local session = require("autodb.session")
-- The plugin's own wrapper, never auto-core.log directly: ADR-0021 §6 gives
-- every auto-family plugin exactly one lua/<plugin>/log.lua so changing the
-- sink is a one-file change rather than a sweep of call sites. Every other
-- autodb module already does this; this one was the outlier.
local log = require("autodb.log")

local M = {}

---Glyph per transaction state, matching `history.STATUS_MARKS` in spirit:
---an unrecognised state renders as unknown, never as success.
---@type table<string, string>
M.STATE_MARKS = {
  opened = "◌",
  commit_started = "⋯",
  unknown_pending = "⋯",
  committed = "·",
  rolled_back = "↩",
  outcome_unresolvable = "?",
}

---state_mark returns the glyph for one transaction state.
---@param state string?
---@return string
function M.state_mark(state)
  return M.STATE_MARKS[state or ""] or "?"
end

---humanize turns a stuck duration in milliseconds into something short.
---
---How long a transaction has been in its current state is the number that
---decides whether to act, so it is shown rather than left as a timestamp the
---reader has to subtract.
---@param ms integer?
---@return string
function M.stuck_for(ms)
  ms = tonumber(ms) or 0
  if ms < 1000 then return string.format("%dms", ms) end
  local secs = math.floor(ms / 1000)
  if secs < 60 then return string.format("%ds", secs) end
  local mins = math.floor(secs / 60)
  if mins < 60 then return string.format("%dm%02ds", mins, secs % 60) end
  return string.format("%dh%02dm", math.floor(mins / 60), mins % 60)
end

---one_line renders a pending entry for a list.
---@param e table
---@return string
function M.one_line(e)
  e = e or {}
  local reason = e.reason
  if reason == nil or reason == "" then reason = "-" end
  return string.format("%s %-20s conn %-4d %-20s stuck %s",
    M.state_mark(e.state), tostring(e.state or "?"), tonumber(e.conn_id) or 0,
    reason, M.stuck_for(e.stuck_ms))
end

---status asks about ONE transaction.
---
---A transaction that is not yours answers exactly as one that never existed,
---so this cannot be used to discover which ids exist.
---@param tx_id string
---@param cb fun(entry: table?, err: table?)
function M.status(tx_id, cb)
  session.authed("tx.status", { tx_id }, session.guarded(function(res, err)
    if err then
      log.debug("txstatus", "tx.status failed: " .. tostring(err.message))
      return cb(nil, err)
    end
    cb(res, nil)
  end))
end

---pending lists YOUR unresolved transactions, oldest first.
---
---Oldest first because the operator question is "what is stuck", and a limit
---that truncated the newest would hide exactly the ones worth seeing.
---@param limit integer?
---@param cb fun(entries: table[]?, err: table?)
function M.pending(limit, cb)
  session.authed("tx.status", { "", limit or 0 }, session.guarded(function(res, err)
    if err then
      log.debug("txstatus", "tx.status pending failed: " .. tostring(err.message))
      return cb(nil, err)
    end
    cb((res or {}).pending or {}, nil)
  end))
end

return M
