-- supplier_pick.lua: pick one supplier at random by weight. Read-only; writes no key.
-- KEYS: 1 meta, 2 cum, 3 weight
-- ARGV: 1 seed, 2..n supplier IDs to exclude this time (optional, for retrying with another supplier after a failure)
-- Returns: {1, supplier_id, weight, version} or {0, err}
-- supplier_pick.lua：按权重随机选一个供应商。只读脚本，不写任何 Key。
-- ARGV: 1 seed, 2..n 本次要排除的供应商 ID（可选，用于调用失败后换一家重试）
-- 返回: {1, supplier_id, weight, version} 或 {0, err}
local meta, err = rs_meta(KEYS[1], 'supplier')
if not meta then return {0, err} end
if meta.enabled == '0' then return {0, 'ERR_SELECTOR_DISABLED'} end
if meta.total <= 0 then return {0, 'ERR_NO_AVAILABLE'} end

local state = rs_seed(ARGV[1])
local sid
if #ARGV <= 1 then
  -- Normal path: binary search by score on the ZSET, O(log N).
  -- 常规路径：ZSET 上按 score 二分，O(log N)。
  local r = rs_rand(state, meta.total)
  sid = redis.call('ZRANGEBYSCORE', KEYS[2], r, '+inf', 'LIMIT', 0, 1)[1]
else
  -- Exclusion path: recover each weight from adjacent score differences, drop the excluded ones, scan linearly, O(N).
  -- 排除路径：由相邻 score 之差还原各自权重，剔除后线性扫描，O(N)。
  local skip = {}
  for i = 2, #ARGV do skip[ARGV[i]] = true end
  local flat = redis.call('ZRANGE', KEYS[2], 0, -1, 'WITHSCORES')
  local ids, cum, prev, acc = {}, {}, 0, 0
  for i = 1, #flat, 2 do
    local score = tonumber(flat[i + 1])
    if not skip[flat[i]] then
      acc = acc + (score - prev)
      ids[#ids + 1] = flat[i]
      cum[#cum + 1] = acc
    end
    prev = score
  end
  if acc <= 0 then return {0, 'ERR_NO_AVAILABLE'} end
  local r = rs_rand(state, acc)
  for i = 1, #ids do
    if cum[i] >= r then sid = ids[i]; break end
  end
end

if not sid then return {0, 'ERR_INCONSISTENT'} end
return {1, sid, tonumber(redis.call('HGET', KEYS[3], sid)) or 0, meta.version}
