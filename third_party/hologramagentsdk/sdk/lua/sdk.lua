-- HologramAgent Lua Plugin SDK
-- Interface contract between Lua plugins and HologramAgent kernel.
-- !impl functions are replaced by Go implementations at runtime.
-- Standalone/debug: pure Lua mock implementations are used.
-- Usage: local sdk = require("sdk")

sdk = {}

-- !impl
-- level: "debug" | "info" | "warn" | "error"
function sdk.log(level, msg)
    print("[lua-plugin] " .. tostring(level) .. ": " .. tostring(msg))
end

-- !impl
-- def: { description="...", parameters={...}, no_memory=true/false, cleaner=function(text)->text }
-- handler: function(args) -> result
function sdk.register_tool(name, def, handler)
    print("[lua-plugin] register_tool: " .. tostring(name))
end

-- !impl
-- stage: "on_input" | "pre_action" | "post_action" | ...
-- scope: nil/"global" (默认) | "own_tools"（仅 before_toolcall/after_toolcall 且工具属于本插件时触发）
function sdk.register_stage(stage, handler, scope)
    print("[lua-plugin] register_stage: " .. tostring(stage) .. " scope=" .. tostring(scope))
end

-- !impl
function sdk.register_api(name)
    print("[lua-plugin] register_api: " .. tostring(name))
end

-- !impl
-- def: { no_memory=true/false, cleaner=function(text)->text }
-- handler: function(args) -> result
function sdk.register_output_channel(name, caps, desc, def, handler)
    print("[lua-plugin] register_output_channel: " .. tostring(name))
end

-- !impl
-- def: { no_memory=true/false, cleaner=function(text)->text }
function sdk.register_input_channel(name, def)
    print("[lua-plugin] register_input_channel: " .. tostring(name))
end

-- !impl
function sdk.get_setting(key)
    return nil
end

-- !impl
function sdk.set_setting(key, value)
    print("[lua-plugin] set_setting: " .. tostring(key))
end

-- !impl
function sdk.inject_text(source, channel, text)
    print("[lua-plugin] inject_text: " .. tostring(source) .. "/" .. tostring(channel))
end

-- !impl
function sdk.inject_interrupt(source, channel, text)
    print("[lua-plugin] inject_interrupt: " .. tostring(source))
end

-- !impl
function sdk.inject_text_no_memory(source, channel, text)
    print("[lua-plugin] inject_text_no_memory: " .. tostring(source))
end

-- !impl
-- opts: { no_memory=bool, context_policy="none"|"prune", cleaner_name=string, priority="L1".."L3" }
-- 零值/缺省 = 记入记忆 + 不裁剪（与三参数版本等价）。
function sdk.inject_text_opts(source, channel, text, opts)
    print("[lua-plugin] inject_text_opts: " .. tostring(source))
end

-- !impl
function sdk.inject_interrupt_opts(source, channel, text, opts)
    print("[lua-plugin] inject_interrupt_opts: " .. tostring(source))
end

-- !impl
-- 同步注入在 Lua 插件中**不可用**：会等本轮回复，而本轮正持有插件锁 ⇒ 必然自锁。
-- 真实内核里恒返回 (nil, err)；这里返回同样的错误，避免离线测试误以为可用。
function sdk.inject_input_sync(source, channel, text)
    return nil, "同步注入在 Lua 插件中不可用：请在事件回调/外部入口用 inject_text/inject_interrupt；确需同步等待请改用 Go 插件。"
end

-- !impl
function sdk.inject_input_sync_opts(source, channel, text, opts)
    return nil, "同步注入在 Lua 插件中不可用：请在事件回调/外部入口用 inject_text/inject_interrupt；确需同步等待请改用 Go 插件。"
end

-- !impl
-- blocks: ContentBlock 数组，见 sdk.inject_input_media。
-- 设置下一轮 tool message 携带的多模态内容块（模型据此看图/听音频）。
function sdk.set_tool_blocks(blocks)
    print("[lua-plugin] set_tool_blocks: " .. tostring(blocks and #blocks or 0))
end

-- !impl
-- blocks 每项：{ type="text", text="..." }
--          | { type="image_url", image_url={ url="...", detail="high" } }
--          | { type="audio_url", audio_url={ url="..." } }
function sdk.inject_input_media(source, channel, text, blocks)
    print("[lua-plugin] inject_input_media: " .. tostring(source))
end

-- !impl
function sdk.inject_input_media_opts(source, channel, text, blocks, opts)
    print("[lua-plugin] inject_input_media_opts: " .. tostring(source))
end

-- !impl
-- 同 sdk.inject_input_sync：Lua 中不可用。
function sdk.inject_input_media_sync(source, channel, text, blocks)
    return nil, "同步注入在 Lua 插件中不可用：请在事件回调/外部入口用 inject_input_media；确需同步等待请改用 Go 插件。"
end

-- !impl
function sdk.inject_input_media_sync_opts(source, channel, text, blocks, opts)
    return nil, "同步注入在 Lua 插件中不可用：请在事件回调/外部入口用 inject_input_media_opts；确需同步等待请改用 Go 插件。"
end

-- !impl
function sdk.inject_interrupt_media(source, channel, text, blocks)
    print("[lua-plugin] inject_interrupt_media: " .. tostring(source))
end

-- !impl
function sdk.inject_interrupt_media_opts(source, channel, text, blocks, opts)
    print("[lua-plugin] inject_interrupt_media_opts: " .. tostring(source))
end

-- !impl
-- 注销输出通道（随资源生灭的动态通道，如远程设备）。返回 (nil, err)。
function sdk.unregister_output_channel(name) return nil, nil end

-- !impl
-- enabled: true/false，崩溃时内核自动拉起
function sdk.set_auto_restart(enabled)
    print("[lua-plugin] set_auto_restart: " .. tostring(enabled))
end

-- ============ graph memory ============
-- !impl
sdk.memory = {}
-- !impl
-- query: string, depth: number -> {entities={...}, relations={...}}
function sdk.memory.recall(query, depth) return {entities={}, relations={}} end
-- !impl
-- triples: { {subject=, relation=, object=, [confidence=], [sentence_text=]} } -> err
function sdk.memory.commit(triples) return nil end
-- !impl
function sdk.memory.introspect() return {} end
-- !impl
function sdk.memory.merge(source, target) return 0 end
-- !impl
-- criteria: {key=value}, hard: boolean
function sdk.memory.purge(criteria, hard) return 0 end

-- ============ document memory ============
-- !impl
sdk.doc = {}
-- !impl
function sdk.doc.query(text, top_k) return {} end
-- !impl
-- doc: { id=, title=, content= }
function sdk.doc.insert(doc) return nil end
-- !impl
-- attachments 每项：{ digest=, mime=, name=, data=<base64> }
function sdk.doc.insert_with_media(doc, attachments) return nil end
-- !impl
function sdk.doc.remove(id) return nil end
-- !impl
function sdk.doc.stats() return {} end

-- ============ knowledge ============
-- !impl
sdk.knowledge = {}
-- !impl
function sdk.knowledge.search(query, limit) return {} end
-- !impl
function sdk.knowledge.add(tag, content) return nil end
-- !impl
function sdk.knowledge.list() return {} end

-- ============ text memory ============
-- !impl
sdk.text_memory = {}
-- !impl
-- evt: { timestamp=, role=, content=, channel= }
function sdk.text_memory.append(evt) return nil end

-- ============ llm ============
-- !impl
sdk.llm = {}
-- !impl
function sdk.llm.list_sources() return {} end
-- !impl
function sdk.llm.set_source(name) return nil end
-- !impl
function sdk.llm.current_source() return nil end

-- ============ social (只读) ============
-- !impl
sdk.social = {}
-- !impl
function sdk.social.get_person(name) return {} end
-- !impl
function sdk.social.get_network(name, depth) return {} end
-- !impl
function sdk.social.get_trait(name, trait) return {value=nil, found=false} end
-- !impl
function sdk.social.get_relations(name) return {} end
-- !impl
function sdk.social.list_persons() return {} end

-- ============ settings (作用域变体) ============
-- !impl
sdk.settings = {}
-- !impl
function sdk.settings.get_core(key) return nil end
-- !impl
function sdk.settings.set_core(key, value) return nil end
-- !impl
function sdk.settings.list_core(prefix) return {} end
-- !impl
function sdk.settings.get_plugin(plugin, key) return nil end
-- !impl
function sdk.settings.set_plugin(plugin, key, value) return nil end
-- !impl
function sdk.settings.list_plugin(plugin, prefix) return {} end
-- !impl
function sdk.settings.list(prefix) return {} end
-- !impl
-- def: { key=, type=, display_name=, description=, category=, options=, default=,
--        min=, max=, step=, required=, secret= }
function sdk.settings.register_def(def) return nil end
-- !impl
function sdk.settings.defs(prefix) return {} end
-- !impl
function sdk.settings.dump() return {} end
-- !impl
function sdk.settings.plugins() return {} end

-- ============ events（只读订阅） ============
-- !impl
-- subscribe(event_type, handler) -> unsubscribe()
-- handler 收到 { type=, source=, timestamp=, payload= }；
-- 回调在其内核事件发布 goroutine 上执行，只做轻量转发，不可阻塞（Lua 单状态 + 互斥锁）。
sdk.events = {}
function sdk.events.subscribe(event_type, handler)
    print("[lua-plugin] events.subscribe: " .. tostring(event_type))
    return function() end
end

-- ============ plugin_mgr ============
-- !impl
sdk.plugin_mgr = {}
function sdk.plugin_mgr.reload_one(name) return nil end
function sdk.plugin_mgr.list_loaded() return {} end
function sdk.plugin_mgr.is_disabled(name) return false end

-- json utils (pure Lua)
sdk.json = {}

function sdk.json.encode(val)
    local ok, result = pcall(function()
        local function _encode(v)
            local t = type(v)
            if t == "string" then
                local s = v:gsub('\\', '\\\\'):gsub('"', '\\"'):gsub('\n', '\\n'):gsub('\r', '\\r'):gsub('\t', '\\t')
                return '"' .. s .. '"'
            elseif t == "number" then
                return tostring(v)
            elseif t == "boolean" then
                return tostring(v)
            elseif t == "table" then
                local keys = {}
                local is_array = true
                local maxn = 0
                for k in pairs(v) do
                    keys[#keys + 1] = k
                    if type(k) ~= "number" or k < 1 or k ~= math.floor(k) then
                        is_array = false
                    end
                    if type(k) == "number" and k > maxn then maxn = k end
                end
                if is_array and #keys >= maxn then
                    local parts = {}
                    for i = 1, maxn do
                        parts[#parts + 1] = _encode(v[i])
                    end
                    return "[" .. table.concat(parts, ",") .. "]"
                else
                    local parts = {}
                    for _, k in ipairs(keys) do
                        parts[#parts + 1] = _encode(tostring(k)) .. ":" .. _encode(v[k])
                    end
                    return "{" .. table.concat(parts, ",") .. "}"
                end
            else
                return "null"
            end
        end
        return _encode(val)
    end)
    if ok then return result end
    return "null"
end

function sdk.json.decode(str)
    local ok, result = pcall(function()
        local pos, _end = 1, #str
        local function skip()
            while pos <= _end and str:sub(pos, pos):match("%s") do pos = pos + 1 end
        end
        local function parse()
            skip()
            if pos > _end then return nil end
            local c = str:sub(pos, pos)
            if c == '"' then
                local s = {}
                pos = pos + 1
                while pos <= _end do
                    local ch = str:sub(pos, pos)
                    if ch == '"' then
                        pos = pos + 1
                        return table.concat(s)
                    elseif ch == '\\' then
                        pos = pos + 1
                        local n = str:sub(pos, pos)
                        if n == '"' then s[#s+1] = '"'
                        elseif n == '\\' then s[#s+1] = '\\'
                        elseif n == '/' then s[#s+1] = '/'
                        elseif n == 'b' then s[#s+1] = '\b'
                        elseif n == 'f' then s[#s+1] = '\f'
                        elseif n == 'n' then s[#s+1] = '\n'
                        elseif n == 'r' then s[#s+1] = '\r'
                        elseif n == 't' then s[#s+1] = '\t'
                        elseif n == 'u' then
                            local hex = str:sub(pos+1, pos+4)
                            pos = pos + 4
                            s[#s+1] = utf8 and utf8.char(tonumber(hex, 16)) or '?'
                        end
                        pos = pos + 1
                    else
                        s[#s+1] = ch
                        pos = pos + 1
                    end
                end
                return table.concat(s)
            elseif c == 't' then pos = pos + 4; return true
            elseif c == 'f' then pos = pos + 5; return false
            elseif c == 'n' then pos = pos + 4; return nil
            elseif c == '{' then
                pos = pos + 1; skip()
                local t = {}
                if str:sub(pos, pos) == '}' then pos = pos + 1; return t end
                while true do
                    skip(); local k = parse(); skip()
                    if str:sub(pos, pos) == ':' then pos = pos + 1 end
                    skip(); t[k] = parse(); skip()
                    local sep = str:sub(pos, pos)
                    if sep == '}' then pos = pos + 1; return t end
                    if sep == ',' then pos = pos + 1 end
                end
            elseif c == '[' then
                pos = pos + 1; skip()
                local t = {}
                if str:sub(pos, pos) == ']' then pos = pos + 1; return t end
                local idx = 1
                while true do
                    skip(); t[idx] = parse(); idx = idx + 1; skip()
                    local sep = str:sub(pos, pos)
                    if sep == ']' then pos = pos + 1; return t end
                    if sep == ',' then pos = pos + 1 end
                end
            else
                local s, e = str:find('^[-%d%.eE]+', pos)
                if s then
                    local num = tonumber(str:sub(s, e))
                    pos = e + 1
                    return num
                end
                return nil
            end
        end
        return parse()
    end)
    if ok then return result end
    return nil
end

-- http utils
sdk.http = {}

-- !impl
function sdk.http.get(url)
    print("[lua-plugin] http.get: " .. tostring(url))
    return {status=200, body='{"mock":true}', headers={}}
end

-- !impl
function sdk.http.post(url, body, content_type)
    print("[lua-plugin] http.post: " .. tostring(url))
    return {status=200, body='{"mock":true}', headers={}}
end

return sdk
