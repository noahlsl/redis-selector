-- lib.lua: shared prelude. Go prepends it to every script at load time so that all scripts use the same implementation.
-- lib.lua：公共前导片段。Go 在加载时把它拼接到每个业务脚本之前，保证各脚本共用同一份实现。
local RS_M = 2147483647 -- 2^31-1 (prime) / 2^31-1（素数）
local RS_A = 48271      -- MINSTD multiplier; RS_A * RS_M < 2^53, so Lua doubles lose no precision / MINSTD 乘数；RS_A * RS_M < 2^53，在 Lua 的 double 下不丢精度

-- Normalize the seed. A missing or non-positive seed falls back to TIME (seconds * 10^6 + microseconds).
-- 归一化种子。seed<=0 或缺失时退化为 TIME（秒 * 10^6 + 微秒）。
local function rs_seed(raw)
  local seed = tonumber(raw) or 0
  if seed <= 0 then
    local t = redis.call('TIME')
    seed = tonumber(t[1]) * 1000000 + tonumber(t[2])
  end
  local state = seed % (RS_M - 1) + 1 -- map into [1, M-1], away from the fixed point 0 / 落入 [1, M-1]，避开 0 这个不动点
  for _ = 1, 3 do state = (state * RS_A) % RS_M end -- warm up to spread adjacent seeds apart / 预热，打散相邻种子
  return state
end

-- Return an integer in [1, n] and the advanced state.
-- 返回 [1, n] 内的整数，以及推进后的状态。
local function rs_rand(state, n)
  state = (state * RS_A) % RS_M
  return math.floor((state - 1) / (RS_M - 1) * n) + 1, state
end

local function rs_now_ms()
  local t = redis.call('TIME')
  return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end

local function rs_hmap(key)
  local flat = redis.call('HGETALL', key)
  local m = {}
  for i = 1, #flat, 2 do m[flat[i]] = flat[i + 1] end
  return m
end

-- Read and validate meta. Returns the meta table, or nil and an error code.
-- 读取并校验 meta。成功返回 meta 表，失败返回 nil 和错误码。
local function rs_meta(k_meta, want_mode)
  local v = redis.call('HMGET', k_meta, 'mode', 'enabled', 'total_weight', 'version', 'min_active')
  if not v[1] then return nil, 'ERR_SELECTOR_NOT_FOUND' end
  if want_mode and v[1] ~= want_mode then return nil, 'ERR_MODE_MISMATCH' end
  return {
    mode = v[1],
    enabled = v[2],
    total = tonumber(v[3]) or 0,
    version = tonumber(v[4]) or 0,
    min_active = math.max(v[1] == 'supplier' and 1 or 0, tonumber(v[5]) or 0),
  }
end

-- Rebuild the cumulative weight table. A candidate qualifies when weight>0, it is enabled, and stock is -1 (unlimited) or >0.
-- IDs are sorted before accumulating so that replicas and replays produce the same result.
-- 重建累积权重表。入选条件：weight>0、未停用、库存为 -1（不限）或 >0。
-- 按 ID 排序后累加，保证主从与重放时结果一致。
local function rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
  local lottery = redis.call('HGET', k_meta, 'mode') == 'lottery'
  local weight, stock, enabled = rs_hmap(k_weight), rs_hmap(k_stock), rs_hmap(k_enabled)
  local ids = {}
  for id in pairs(weight) do ids[#ids + 1] = id end
  table.sort(ids)
  redis.call('DEL', k_cum)
  local acc, active = 0, 0
  for _, id in ipairs(ids) do
    local w = tonumber(weight[id]) or 0
    local s = lottery and tonumber(stock[id]) or nil
    if not lottery then s = -1 end
    if w > 0 and enabled[id] == '1' and s and s == math.floor(s) and (s == -1 or s > 0) then
      acc = acc + w
      active = active + 1
      redis.call('ZADD', k_cum, acc, id)
    end
  end
  redis.call('HSET', k_meta,
    'total_weight', acc,
    'active_count', active,
    'updated_at', string.format('%.0f', rs_now_ms()))
  local version = redis.call('HINCRBY', k_meta, 'version', 1)
  return active, acc, version
end
