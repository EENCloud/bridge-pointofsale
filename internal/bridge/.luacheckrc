-- Luacheck configuration for bridge-devices-pos EEN Lua script

-- Standard Lua globals
std = "lua53"

-- Allow EEN bridge runtime globals
globals = {
    -- Standard Lua
    "print",
    "require",
    "module",
    "package",
    
    -- EEN Bridge runtime globals (from actual usage)
    "EEN",
    "EENLog",
    "EENInfo",
    "EENEnv", 
    "EENCurl",
    "EENETag",
    "MPack",
    
    -- EEN Bridge functions that get injected/called by runtime
    "get_events",
    "install",
    "schema",
    "start", 
    "stop",
    
    -- Script globals (defined in the script)
    "ScriptName",
    "Esn",
    "EsnStr", 
    "Running",
    "Settings",
    "Subscriptions",
    "E",
}

-- Ignore certain warnings common in bridge scripts
ignore = {
    "212",  -- Unused argument (common in callback functions)
    "213",  -- Unused local variable (often used for debugging)
    "542",  -- Empty if branch (common in conditional logging) 
    "611",  -- Line contains only whitespace
    "612",  -- Line contains trailing whitespace
    "614",  -- Trailing whitespace in comments
    "131",  -- Unused global variable (for bridge API functions)
}

-- File-specific configurations
files = {
    ["internal/bridge/point_of_sale.lua"] = {
        -- Allow setting these as globals (they're bridge API functions called by EEN runtime)
        allow_defined_top = true,
        -- These functions are part of the bridge API contract
        globals = {
            "install",  -- Bridge API: called on install
            "schema",   -- Bridge API: called for schema
            "start",    -- Bridge API: called on start  
            "stop",     -- Bridge API: called on stop
        }
    }
}

-- Configuration for bridge scripts
max_line_length = 120
max_cyclomatic_complexity = 20  -- Bridge scripts can be complex due to state management
