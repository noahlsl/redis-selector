-- selector_mutate.lua: add, update, remove, enable or disable one candidate, plus the selector master switch. Shared by both modes.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
-- ARGV: 1 op, 2 id, 3 weight, 4 stock or stock delta, 5 item_json, 6 max_candidates
-- Returns: {1, active_count, total_weight, version} or {0, err}
-- selector_mutate.lua：单个候选的增删改与启停，以及选择器总开关。两种模式共用。
-- ARGV: 1 op, 2 id, 3 weight, 4 stock 或库存增量, 5 item_json, 6 max_candidates
-- 返回: {1, active_count, total_weight, version} 或 {0, err}
local k_meta, k_cum, k_weight, k_stock, k_enabled, k_items =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
local op, id = ARGV[1], ARGV[2]

local meta, err = rs_meta(k_meta, nil)
if not meta then return {0, err} end

local function done()
  local active, total, version = rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
  return {1, active, total, version}
end

local function valid_weight(raw)
  local w = tonumber(raw)
  if w and w >= 1 and w == math.floor(w) then return w end
  return nil
end

if op == 'rebuild' then return done() end

if op == 'selector_enable' or op == 'selector_disable' then
  redis.call('HSET', k_meta, 'enabled', op == 'selector_enable' and '1' or '0')
  return done()
end

local exists = redis.call('HEXISTS', k_weight, id) == 1

-- add is an idempotent upsert: an existing candidate gets its weight and attributes overwritten and is re-enabled.
-- add 是幂等的 upsert：已存在则覆盖权重与属性，并重新启用。
if op == 'add' then
  local w = valid_weight(ARGV[3])
  if not w then return {0, 'ERR_INVALID_WEIGHT'} end
  if (not exists) and redis.call('HLEN', k_weight) >= (tonumber(ARGV[6]) or 1000) then
    return {0, 'ERR_TOO_MANY_CANDIDATES'}
  end
  if meta.mode == 'lottery' then
    local s = tonumber(ARGV[4])
    if (not s) or s < -1 or s ~= math.floor(s) then return {0, 'ERR_INVALID_STOCK'} end
    if not exists then redis.call('HSET', k_stock, id, s) end
  end
  redis.call('HSET', k_weight, id, w)
  redis.call('HSET', k_items, id, ARGV[5])
  redis.call('HSET', k_enabled, id, '1')
  return done()
end

if not exists then return {0, 'ERR_CANDIDATE_NOT_FOUND'} end

if op == 'set_weight' then
  local w = valid_weight(ARGV[3])
  if not w then return {0, 'ERR_INVALID_WEIGHT'} end
  redis.call('HSET', k_weight, id, w)
  return done()
end

if op == 'enable' then
  redis.call('HSET', k_enabled, id, '1')
  return done()
end

if op == 'disable' or op == 'remove' then
  -- Last-active protection: the active count after the operation must not drop below min_active (1 by default in supplier mode).
  -- Only candidates currently in the pool are guarded; a disabled candidate can still be removed.
  -- 末位保护：操作后可用数量不得低于 min_active（供应商模式默认 1）。
  -- 只拦截“当前正在参与选择”的候选，已停用的候选仍可删除。
  if redis.call('ZSCORE', k_cum, id) and redis.call('ZCARD', k_cum) - 1 < meta.min_active then
    return {0, 'ERR_LAST_ACTIVE_PROTECTED'}
  end
  if op == 'disable' then
    redis.call('HSET', k_enabled, id, '0')
  else
    redis.call('HDEL', k_weight, id)
    redis.call('HDEL', k_stock, id)
    redis.call('HDEL', k_enabled, id)
    redis.call('HDEL', k_items, id)
  end
  return done()
end

if op == 'incr_stock' then
  if meta.mode ~= 'lottery' then return {0, 'ERR_MODE_MISMATCH'} end
  local cur = tonumber(redis.call('HGET', k_stock, id) or '-1')
  local delta = tonumber(ARGV[4])
  if cur == -1 then return {0, 'ERR_STOCK_UNLIMITED'} end
  if (not delta) or delta ~= math.floor(delta) or cur + delta < 0 or cur + delta > 9007199254740992 then
    return {0, 'ERR_INVALID_STOCK'}
  end
  redis.call('HINCRBY', k_stock, id, delta)
  return done()
end

return {0, 'ERR_UNKNOWN_OP'}
