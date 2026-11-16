-- selector_query.lua: read the full state of a selector for admin consoles and local snapshots. Read-only.
-- KEYS: 1 meta, 2 weight, 3 stock, 4 enabled, 5 items
-- Returns: {1, json} or {0, err}. Structured data travels as a JSON string because some clients flatten nested arrays.
-- selector_query.lua：读取选择器全貌，供管理后台与本地快照使用。只读脚本。
-- 返回: {1, json} 或 {0, err}。用 JSON 字符串承载结构化数据，避免嵌套数组在部分客户端里被压扁。
local meta = rs_hmap(KEYS[1])
if not meta.mode then return {0, 'ERR_SELECTOR_NOT_FOUND'} end
return {1, cjson.encode({
  meta = meta,
  weight = rs_hmap(KEYS[2]),
  stock = rs_hmap(KEYS[3]),
  enabled = rs_hmap(KEYS[4]),
  items = rs_hmap(KEYS[5]),
})}
