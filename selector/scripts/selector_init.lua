-- selector_init.lua: create a selector, or write candidates in bulk.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
-- ARGV: 1 mode, 2 min_active, 3 replace (1 = replace all, 0 = merge), 4 max_candidates, then 4 values per candidate: id, weight, stock, item_json
-- Returns: {1, active_count, total_weight, version} or {0, err}
-- selector_init.lua：创建选择器，或批量写入候选。
-- ARGV: 1 mode, 2 min_active, 3 replace(1=整体替换, 0=合并), 4 max_candidates，之后每个候选 4 个参数: id, weight, stock, item_json
-- 返回: {1, active_count, total_weight, version} 或 {0, err}
local k_meta, k_cum, k_weight, k_stock, k_enabled, k_items =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
local mode = ARGV[1]

local old = redis.call('HGET', k_meta, 'mode')
if old and old ~= mode then return {0, 'ERR_MODE_MISMATCH'} end

local replace = ARGV[3] == '1'
local count = replace and 0 or redis.call('HLEN', k_weight)
local active = replace and 0 or redis.call('ZCARD', k_cum)
local seen = {}
for i = 5, #ARGV, 4 do
  local id = ARGV[i]
  if seen[id] then return {0, 'ERR_INVALID_ARGUMENT'} end
  seen[id] = true
  if replace or redis.call('HEXISTS', k_weight, id) == 0 then
    count = count + 1
    active = active + 1
  end
end
if count > tonumber(ARGV[4]) then return {0, 'ERR_TOO_MANY_CANDIDATES'} end
if mode == 'supplier' and (tonumber(ARGV[2]) < 1 or active < tonumber(ARGV[2])) then
  return {0, 'ERR_LAST_ACTIVE_PROTECTED'}
end
if replace then
  redis.call('DEL', k_cum, k_weight, k_stock, k_enabled, k_items)
end

for i = 5, #ARGV, 4 do
  local id = ARGV[i]
  redis.call('HSET', k_weight, id, ARGV[i + 1])
  redis.call('HSET', k_items, id, ARGV[i + 3])
  -- Merging keeps the existing disabled state and remaining stock, so a running campaign is never reset to its initial stock.
  -- 合并模式下保留已有的停用状态与剩余库存，避免把进行中的活动库存重置回初始值。
  redis.call('HSETNX', k_enabled, id, '1')
  if mode == 'lottery' then
    redis.call('HSETNX', k_stock, id, ARGV[i + 2])
  end
end

redis.call('HSET', k_meta, 'schema', '1', 'mode', mode, 'min_active', ARGV[2])
redis.call('HSETNX', k_meta, 'enabled', '1')
local active, total, version = rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
return {1, active, total, version}
