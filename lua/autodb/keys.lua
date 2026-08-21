---autodb key surface — the canonical strings, in one place.
---
---Both the keymap definitions and every message that TELLS a user which
---key to press read from here. A notification that says "press
---<leader>DX" while the binding is something else is worse than no
---notification, and that drift is only avoidable if there is one source
---for the string ([[shared-resolver-single-source-of-truth]]).
---@module 'autodb.keys'

local M = {}

---PREFIX is the dbase command prefix (ADR-0058 §3.6). `<leader>d` is
---DAP's; `<leader>D` was free.
M.PREFIX = "<leader>D"

M.LOGIN       = M.PREFIX .. "l"  -- sign in: retry, or switch user
M.WORKSPACE   = M.PREFIX .. "w"  -- choose or create a workspace
M.HISTORY     = M.PREFIX .. "h"  -- history modal
M.RUN_BUFFER  = M.PREFIX .. "r"  -- execute the current .sql buffer
M.RUN_VISUAL  = M.PREFIX .. "R"  -- execute the selection
M.CONNECTION  = M.PREFIX .. "c"  -- choose workspace, then connection
M.MAINTENANCE = M.PREFIX .. "X"  -- prompt: restart / refresh / reset

return M
