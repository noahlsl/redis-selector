-- lottery_draw.lua: weighted draw; limited prizes have their stock deducted in the same script.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, [6 idem]
-- ARGV: 1 seed, 2 idem_ttl_sec
-- Returns: {1, candidate_id, remain_stock (-1 = unlimited), version, replayed (0/1)} or {0, err}
-- lottery_draw.lua：按权重抽奖，限量奖品在同一脚本内扣减库存。
-- 返回: {1, candidate_id, remain_stock(-1 表示不限), version, replayed(0/1)} 或 {0, err}
local k_meta, k_cum, k_weight, k_stock, k_enabled, k_idem =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]

if k_idem and (not tonumber(ARGV[2]) or not string.match(ARGV[2], '^[1-9]%d*$') or tonumber(ARGV[2]) > 9223372036) then
  return {0, 'ERR_INVALID_ARGUMENT'}
end

-- Idempotency: a repeated request with the same key returns the first result and deducts nothing.
-- 幂等：同一个幂等键重复请求，直接返回首次结果，不再扣库存。
if k_idem then
  local saved = redis.call('GET', k_idem)
  if saved then
    local p = cjson.decode(saved)
    return {1, p[1], p[2], p[3], 1}
  end
end

local meta, err = rs_meta(k_meta, 'lottery')
if not meta then return {0, err} end
if meta.enabled == '0' then return {0, 'ERR_SELECTOR_DISABLED'} end
if meta.total <= 0 then return {0, 'ERR_NO_AVAILABLE'} end

if not tonumber(ARGV[1]) or tonumber(ARGV[1]) <= 0 then return {0, 'ERR_INVALID_ARGUMENT'} end
local r = rs_rand(rs_seed(ARGV[1]), meta.total)
local cid = redis.call('ZRANGEBYSCORE', k_cum, r, '+inf', 'LIMIT', 0, 1)[1]
local raw = cid and redis.call('HGET', k_stock, cid)
local stock = raw and tonumber(raw)
if cid and (not stock or stock < -1 or stock ~= math.floor(stock)) then
  return {0, 'ERR_INVALID_STOCK'} -- a missing stock field must not be treated as unlimited / 库存缺失不能当作不限量
end

-- Self-heal: meta and cum disagree, or a zero-stock prize is still in cum. Rebuild and let Go retry once.
-- 自愈：meta 与 cum 不一致，或零库存奖品仍留在 cum 中。重建后让 Go 重试一次。
if (not cid) or stock == 0 then
  rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
  return {0, 'ERR_INCONSISTENT'}
end

local remain, version = -1, meta.version
if stock > 0 then
  remain = redis.call('HINCRBY', k_stock, cid, -1)
  if remain == 0 then
    -- Sold out: the whole prefix sum must be rebuilt; a bare ZREM would let the next prize absorb this weight range.
    -- 售罄：必须整体重建前缀和，只 ZREM 会让后一个奖品吞掉它的权重区间。
    local _, _, v = rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
    version = v
  end
end

if k_idem then
  redis.call('SET', k_idem, cjson.encode({cid, remain, version}), 'EX', tonumber(ARGV[2]) or 86400)
end
return {1, cid, remain, version, 0}
