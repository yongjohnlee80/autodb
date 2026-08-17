---`:checkhealth autodb` entry point. The report lives in `autodb.health`
---so the plugin has one implementation rather than a copy per surface.
---@module 'autodb.health'
return { check = function() require("autodb").health() end }
