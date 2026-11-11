local adapter = {}

adapter.name = "deepseek"
adapter.version = "2.1.0"
adapter.endpoint = "/chat/completions"
adapter.headers = {}

-- ── 用量归一化（transform_response 与 transform_stream_chunk 共用）──
--
-- 输出键名对齐 homed 的 agentAPI.TokenUsage（json tag）：
--   prompt / completion / total / cache_read / cache_miss / cache_reported /
--   reasoning_tokens
-- 所以这张表会被 json.Unmarshal 直接吃进 StreamChunk.Usage。
--
-- ★ 为何必须透传：适配器是**归一化层**，上游给的用量只有它看得见。
--   此前它只搬 prompt/completion/total，缓存命中与推理 token 在归一化时被丢掉
--   —— 而这两个正是「缓存省了多少、思考花了多少」的唯一来源。
--   丢掉之后内核只剩估算，永远答不出真实成本。
--
-- ★ 两种上游形态都认（llmsproxy 会把各家的都归一成第一种）：
--   · OpenAI v2：usage.prompt_tokens_details.cached_tokens
--   · DeepSeek 遗留：usage.prompt_cache_hit_tokens / prompt_cache_miss_tokens
local function usage_to_unified(u)
    if type(u) ~= "table" then return nil end
    local out = {
        prompt = u.prompt_tokens or u.prompt or 0,
        completion = u.completion_tokens or u.completion or 0,
        total = u.total_tokens or u.total or 0
    }
    local details = u.prompt_tokens_details
    if type(details) == "table" then
        out.cache_read = details.cached_tokens or 0
        -- ★ 只要上游**给了这个对象**就标记「报了缓存」——即使命中为 0。
        --   它必须与「没给」区分：前者是「这次没命中」，后者是「不知道」。
        --   混起来会把无数据画成 0% 命中率，让人去优化一个本来没开的功能。
        out.cache_reported = true
    elseif (u.prompt_cache_hit_tokens or 0) > 0 then
        out.cache_read = u.prompt_cache_hit_tokens
        out.cache_reported = true
    end
    if (u.prompt_cache_miss_tokens or 0) > 0 then
        out.cache_miss = u.prompt_cache_miss_tokens
    end
    local cd = u.completion_tokens_details
    if type(cd) == "table" and (cd.reasoning_tokens or 0) > 0 then
        out.reasoning_tokens = cd.reasoning_tokens
    end
    -- total 缺失时补出来，避免 total=0 而分量为正的自相矛盾记录。
    if out.total == 0 then out.total = out.prompt + out.completion end
    return out
end

function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    req.model = req.model or "deepseek-chat"
    req.stream = req.stream or false

    -- ── thinking 开关：必须是**顶层**字段，不能包在 extra_body 里 ──
    --
    -- 官方文档示例全用 OpenAI SDK：extra_body={"thinking": {...}}
    -- 那是 **SDK 层参数** —— SDK 会把它摊平进 HTTP 请求体顶层，
    -- 真实发出去的是 {"thinking": {...}}。而本适配器是把 JSON **裸发**给
    -- api.deepseek.com 的，再包一层 extra_body 服务端不认识，直接忽略
    -- ⇒ thinking 保持默认（**开启**）⇒ 带 tools 的请求就要求回传
    -- reasoning_content，否则 400。
    --
    -- 修正前写的是 req.extra_body.thinking，所以即使内核
    -- thinking_enabled=false（发出 disable_thinking=true）也从未真正关掉思考。
    --
    -- 为何值得关：deepseek-flash 的 thinking 默认开启，显式关闭省掉整段 CoT
    -- 的 token；若用户确实要思考，就不会配 thinking_enabled=false。
    if req.disable_thinking then
        req.thinking = { type = "disabled" }
    end
    req.disable_thinking = nil
    -- 内核从不发 extra_body；万一将来发了，也不该把 SDK 层的壳透传给上游。
    req.extra_body = nil

    -- ── reasoning_content 回传兜底（2026-11-11 生产故障）──
    --
    -- 上游报：
    --   all 2 providers failed, last error: api error 400:
    --   "The reasoning_content in the thinking mode must be passed back to the API."
    --
    -- DeepSeek 官方规则（api-docs.deepseek.com/guides/thinking_mode）：
    --   请求**带 tools** 时，所有历史轮的 reasoning_content 都必须传回，
    --   并会拼进上下文；不带 tools 时不要求。
    --
    -- 内核侧已修（ContextEvent.ReasoningContent + renderL1Messages 回传），
    -- 但**存量会话**的 context.json 是在修复前写的，那些事件根本没这个字段。
    -- 不补就是个死结：历史缺字段 ⇒ 400 ⇒ 会话永久不可用（重启也救不回来，
    -- 因为缺的字段在磁盘上的旧记录里）。
    --
    -- 修法：给**每一条 assistant 消息**补齐该字段（缺失时补空串）。
    -- 为什么空串是合法的：字段的语义是"上一轮的思考内容"，空串如实表达
    -- "这一轮没有可回传的思考"（其他主流实现的同款修复也是补空串：
    -- langchain #37179、n8n #34924、opencode #24722）。
    --
    -- 只给 assistant 补：system/user/tool 消息不接受该字段，乱发可能被拒。
    if type(req.messages) == "table" then
        local has_tools = type(req.tools) == "table" and #req.tools > 0
        for _, m in ipairs(req.messages) do
            if type(m) == "table" and m.role == "assistant" and m.reasoning_content == nil then
                -- 只在带 tools 的请求里补（无 tools 时上游不要求，也不想平白
                -- 往上下文里塞一个空字段）。但如果模型名显式是 reasoner，
                -- 它可能无条件校验，所以两条都补。
                if has_tools or string.find(tostring(req.model), "reasoner", 1, true) ~= nil then
                    m.reasoning_content = ""
                end
            end
        end
    end

    return json.encode(req)
end

function adapter.transform_response(raw_body)
    local ok, resp = pcall(json.decode, raw_body)
    if not ok or resp == nil then return raw_body end

    local unified = {
        content = "",
        finish_reason = "",
        token_usage = { prompt = 0, completion = 0, total = 0 }
    }

    if type(resp.usage) == "table" then
        unified.token_usage = usage_to_unified(resp.usage)
    end

    if type(resp.choices) == "table" and #resp.choices > 0 then
        local ch = resp.choices[1]
        if type(ch.message) == "table" then
            unified.content = ch.message.content or ""
            if ch.message.reasoning_content then
                unified.reasoning_content = ch.message.reasoning_content
            end
            if type(ch.message.tool_calls) == "table" then
                local tcs = {}
                for _, tc in ipairs(ch.message.tool_calls) do
                    local fn = tc["function"]
                    local name = tc.name
                    local raw_args = tc.arguments
                    if type(fn) == "table" then
                        name = fn.name or name
                        raw_args = fn.arguments or raw_args
                    end
                    local args = {}
                    if type(raw_args) == "table" then
                        args = raw_args
                    elseif type(raw_args) == "string" and raw_args ~= "" then
                        local args_ok, decoded = pcall(json.decode, raw_args)
                        if args_ok and type(decoded) == "table" then
                            args = decoded
                        elseif args_ok then
                            args = { value = decoded }
                        else
                            args = { raw = raw_args }
                        end
                    end
                    if name ~= nil and name ~= "" then
                        table.insert(tcs, {
                            id = tc.id,
                            type = tc.type or "function",
                            name = name,
                            arguments = args
                        })
                    end
                end
                unified.tool_calls = tcs
            end
        end
        unified.finish_reason = ch.finish_reason or ""
    end

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end
    -- 用量可能在任意帧上（内容帧、末帧、纯心跳帧），同一趟里一并取出。
    local usg = usage_to_unified(chunk.usage)
    if not chunk.choices or #chunk.choices == 0 then
        -- 纯 usage 心跳帧（OpenAI 开 include_usage 时末帧：choices 为空、只有 usage）。
        -- 有用量就带出去；没有则返回 "" 交回 Go 侧的标准解析。
        if usg then return json.encode({ usage = usg }) end
        return ""
    end
    local delta = chunk.choices[1].delta or {}
    local fr = chunk.choices[1].finish_reason
    local unified = {
        content = delta.content or "",
        done = (fr ~= nil)
    }
    if usg then unified.usage = usg end
    if delta.reasoning_content then
        unified.reasoning_content = delta.reasoning_content
    end
    -- ★ 必须处理流式 tool_calls —— 此前只透 content/done，导致 deepseek 源
    --   在**流式**模式下工具调用全部丢失，模型调不动任何工具且无任何报错。
    --
    --   为什么难发现：非流式路径（transform_response）是好的，所以端到端
    --   手工测试也过；而内核的 tool call 循环默认走流式。
    --   功能判据（core 包的批内测试）直接构造 Go 结构体，绕过适配器。
    --
    -- 形态与 openai.lua 一致：OpenAI 兼容流式格式
    -- {function:{name,arguments}, id, type, index} → homed 扁平结构
    -- {id, type, name, raw_arguments, stream_index}。
    if delta.tool_calls then
        local tcs = {}
        for _, tc in ipairs(delta.tool_calls) do
            local fn = tc["function"]
            local name = (type(fn) == "table" and fn.name) or tc.name or ""
            local raw_args = ""
            if type(fn) == "table" and type(fn.arguments) == "string" then
                raw_args = fn.arguments
            elseif type(tc.arguments) == "string" then
                raw_args = tc.arguments
            end
            -- 不能按 name 过滤：流式续传片 name 为空但携带 arguments，
            -- 内核 accumulateStream 按 stream_index 分桶并累积
            table.insert(tcs, {
                id = tc.id or "",
                type = tc.type or "function",
                name = name,
                raw_arguments = raw_args,
                -- 透传上游分片 index：并行多工具调用时内核按它区分归属桶
                stream_index = tc.index or 0
            })
        end
        unified.tool_calls = tcs
    end
    return json.encode(unified)
end

return adapter
