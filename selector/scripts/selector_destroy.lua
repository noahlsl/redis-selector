-- selector_destroy.lua: delete all keys of a selector. Idempotency records expire by their own TTL.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
-- selector_destroy.lua：删除选择器的全部 Key。幂等键靠 TTL 自行过期。
return {1, redis.call('DEL', unpack(KEYS))}
