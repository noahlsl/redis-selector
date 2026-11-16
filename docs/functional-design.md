# Distributed Weighted Selector: Functional Design Document

| Item | Description |
| --- | --- |
| Component | redis-selector |
| Document type | Functional and architecture design |
| Document version | v2.2 (implementation and tests) |
| Target language | Go 1.18 and later |
| State storage | Redis 5.0 and later, Standalone and Cluster |
| Use cases | Lottery draws with stock limits; weighted random supplier selection and maintenance |

---

## 0. Revision History

v2.2 puts the code into the repository and adds tests. It also makes these fixes: a new `Rebuild` maintenance method (the earlier document asked operators to rebuild the index, but no interface existed for it); a stock ceiling of 2^53, above which Lua cannot compare values exactly; `MinActive` in a merge write is no longer limited by the number of candidates in that call; the enabled state reported by `Describe` now uses the same rule as the rebuild function; and `Add` in supplier mode no longer validates the stock field, which that mode does not use.

v2.1 fixed the following: stock that was abnormal could still be issued; re-registering a prize restored its stock; the last-active lower bound could be bypassed; merge writes could exceed the candidate limit; and a sub-second TTL failed after stock had been deducted. The measurement table in section 13 is the v2.0 record, kept as history.

v2.0 was a full review of v1.1. The table below lists each problem found and how it was handled. Scripts and Go code were measured as described in section 13.

| No. | Problem in v1.1 | Impact | How v2.0 handles it |
| --- | --- | --- | --- |
| 1 | LCG uses `1103515245 * state`, and the product exceeds 2^53 | Lua numbers are doubles, so the multiplication loses precision and the random distribution is skewed | Replaced with MINSTD (multiplier 48271, modulus 2^31-1); products always stay below 2^53 |
| 2 | After a prize sells out, only `ZREM` runs; the cumulative weights are not rebuilt and `total_weight` is not updated | The prize after it absorbs the weight range, so the probabilities are wrong | Sell-out, enable/disable, add/remove and reweight all go through the same rebuild function |
| 3 | `supplier_pick` reads `w:<id>` from `meta`, a field that is never written | The returned weight is always 0 | Original weights are stored in their own `weight` hash, which the script reads directly |
| 4 | Last-active protection relies on the redundant field `meta.active_count` | Once the redundant value goes stale, protection either fails or blocks wrongly | Reads the real members of `cum` and supports a configurable lower bound `min_active` |
| 5 | `UpsertCandidates` exists only as an interface, with no script; it writes through several commands | Other instances can read half-written data during initialization | Added the `selector_init` script, which writes and rebuilds in one call |
| 6 | Interface signatures do not match the implementation; lottery errors were mapped to supplier errors; `IdemKey` was a field with no implementation | The design could not be implemented as written | Interface, implementation and error codes were rewritten together and compile; the idempotency key is implemented inside the lottery script |
| 7 | `RedisClient` requires 7 methods; signatures such as `Hdel` in the go-zero example do not match the real ones | The adapters could not be written | Reduced to `Eval` and `EvalSha`; each of the four adapters is about 20 lines |
| 8 | Warm-up depends on `SCRIPT LOAD` | In Cluster it loads on one node only | The SHA1 is computed locally; when `EVALSHA` returns `NOSCRIPT`, the call switches to `EVAL` |
| 9 | `KeyPrefix` can be overridden per call | Reads and writes for one selector can land on different keys | Prefix and namespace are set only at construction time |
| 10 | No security design | Key injection, unauthorized maintenance, predictable results and duplicate deductions were not covered | Added section 9 |
| 11 | No extension design | New modes or strategies require changes to the core | Added section 10 |
| 12 | No reweight, stock adjustment, query, destroy, or retry with another supplier | Incomplete maintenance capability | Added `SetWeight`, `AddStock`, `Describe`, `Destroy` and `WithExclude` |

The `gofarm` named in the requirements is GoFrame's Redis client (`github.com/gogf/gf/v2/database/gredis`). Section 7.4 gives the adapter for it.

---

## 1. Background and Goals

The component provides distributed weighted random selection for two kinds of business:

1. **Lottery with stock limits (lottery)**: prizes are drawn by weight. A prize can be limited or unlimited. For a limited prize, selecting it and deducting its stock happen in the same Lua script.
2. **Weighted supplier selection (supplier)**: a group of downstream suppliers, each with a weight. Each use picks one at random by weight. Operators can disable, enable, remove and re-add a single supplier, and change weights. Changes take effect on every instance immediately.

Supplier mode has one hard rule, called last-active protection. When only one active supplier is left in a selector, a request to disable or remove it is rejected with `ErrLastActiveProtected`, and that supplier keeps serving.

The component does not include automatic circuit breaking or health probing. Callers bring suppliers online and offline explicitly through the maintenance interface.

Design principles:

- **Decoupled from the client**: the core package depends only on the `RedisClient` interface, which has two methods. Each of the four client libraries has one adapter.
- **Atomicity on the Redis side**: selection, stock deduction, maintenance and rebuilding all run as Lua scripts. The application takes no locks.
- **Cluster friendly**: all keys of one selector share one hash tag, and each script touches only the keys declared in `KEYS`.
- **Split interfaces**: the hot business path receives only `Picker` or `Drawer`. Maintenance is injected separately through `Admin`.

---

## 2. Architecture Overview

```text
Business code                      Operators / admin console
Pick(), Draw()                     Init, Add, Disable, Enable, Remove, ...
        |                                    |
        v                                    v
  Picker / Drawer                        Admin
        |                                    |
        +-----------------+------------------+
                          v
          selector core: input validation, key building,
          seeds, EVALSHA with NOSCRIPT fallback,
          reply parsing, Observer callbacks
                          |
                          v
          RedisClient (Eval / EvalSha)
                          |
     +------------+-------+-------+------------+
  go-redis/v9  go-redis/v8   go-zero        GoFrame
                          |
                          v
          Redis (Standalone / Cluster)
          lib.lua + 6 business scripts
```

| Layer | Responsibility |
| --- | --- |
| Interfaces | Three interfaces, `Picker`, `Drawer` and `Admin`, split by caller |
| Core | Validates input, builds keys, generates seeds, runs scripts, parses replies, calls `Observer` |
| Adapters | Forward `Eval` and `EvalSha` to a specific client; no business logic |
| Lua scripts | Selection, stock deduction, idempotency, maintenance, last-active protection, rebuilding of the cumulative weights |


---

## 3. Data Structures

### 3.1 Key Naming

```text
[<KeyPrefix>]{rs:<namespace>:<selector_id>}:<suffix>
```

- `KeyPrefix` is optional, is set at construction time, and is empty by default. Passing `pay:prod` becomes `pay:prod:` automatically.
- `{rs:<namespace>:<selector_id>}` is the hash tag. Redis Cluster computes the slot only from the content of the first pair of braces, so all keys of one selector land in one slot, and the prefix does not affect the slot.
- `namespace`, `selector_id`, candidate IDs and idempotency keys may contain only `[A-Za-z0-9_.-]`, with a length of 1 to 64. The prefix may also contain colons, with a length of up to 64. Section 9.2 explains why.

| Key | Type | Content |
| --- | --- | --- |
| `...:meta` | Hash | Selector metadata, see 3.2 |
| `...:cum` | ZSET | Members are candidate IDs; scores are cumulative weights. Only currently selectable candidates are included |
| `...:weight` | Hash | Candidate ID to original weight. Every registered candidate is here, including disabled and sold-out ones |
| `...:enabled` | Hash | Candidate ID to `1` or `0` |
| `...:stock` | Hash | Lottery mode only. Candidate ID to remaining stock; `-1` means unlimited |
| `...:items` | Hash | Candidate ID to JSON (`name` and `ext`). Used for display only; the hot path does not read it |
| `...:idem:<key>` | String | Lottery idempotency record, with a TTL |

Examples:

```text
{rs:lottery:spring2026}:cum
pay:prod:{rs:supplier:payment}:meta
pay:prod:{rs:supplier:payment}:cum
pay:prod:{rs:supplier:payment}:idem:order-10086
```

### 3.2 meta Fields

| Field | Description |
| --- | --- |
| `schema` | Data structure version; currently `1` |
| `mode` | `lottery` or `supplier`; cannot be changed after creation |
| `enabled` | Master switch of the selector. When `0`, Pick and Draw return `ErrSelectorDisabled` |
| `min_active` | Lower bound on the number of active candidates. Defaults to 1 in supplier mode and 0 in lottery mode |
| `total_weight` | Sum of the weights in `cum`; written by the rebuild function |
| `active_count` | Number of candidates in `cum`; written by the rebuild function and used only for display and monitoring |
| `version` | Incremented on every rebuild; used for local cache invalidation and troubleshooting |
| `updated_at` | Millisecond timestamp of the last rebuild, taken from Redis `TIME` |

### 3.3 Cumulative Weights and Eligibility

The score of each member in `cum` is the weight accumulated in ascending candidate ID order. To select, the script draws an integer `r` in `[1, total_weight]`, then takes the first member whose score is at least `r`. The cost is O(log N).

```text
A(70)  B(30)            cum: A -> 70, B -> 100
r in 1..70 selects A; r in 71..100 selects B
```

A candidate enters `cum` only when all three conditions hold: its weight is greater than 0, `enabled` is not `0`, and its stock is `-1` or greater than 0. Both modes share these rules. Supplier mode has no stock, so it is equivalent to unlimited. Any change to one of these conditions triggers a full rebuild, and the rebuild runs in the same script as the write that caused it.

### 3.4 Limits

| Item | Default limit | Reason |
| --- | --- | --- |
| Number of candidates | 1000 | A rebuild is O(N log N) and runs on Redis's single thread, so its cost must be bounded |
| Single weight | 1,000,000 | The sum of weights stays below 10^9, far under 2^53, so accumulation loses no precision |
| JSON of `name` plus `ext` | 4096 bytes | Prevents `items` from growing without bound |

Random numbers have a resolution of 2^31. The relative error in a candidate's probability is roughly the total weight divided by 2^31. When the total weight is at most 10^6, the error is below 0.05%. Set weights as percentages or basis points.

---

## 4. Interface Definitions

### 4.1 Types, Interfaces, Configuration and Errors

`RedisClient` has only two methods, because every read and write goes through a script. `Selector` is split into three small interfaces: business code depends on `Picker` or `Drawer`, and the maintenance entry point depends on `Admin`.

```go
package selector

import (
	"context"
	"errors"
	"time"
)

// RedisClient is everything this package needs from Redis. All reads and writes go through Lua, so two methods are enough.
// Contract: script replies are flat arrays of scalars; adapters return them untouched ([]any and []string are both accepted).
type RedisClient interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error)
}

type Mode string

const (
	ModeLottery  Mode = "lottery"
	ModeSupplier Mode = "supplier"
)

// Candidate is a prize or a supplier.
type Candidate struct {
	ID     string
	Name   string
	Weight int64             // 1..MaxWeight (see WithMaxWeight)
	Stock  int64             // lottery only: -1 unlimited, >=0 stock; ignored in supplier mode
	Ext    map[string]string // display-only extension fields; never store secrets here
}

// Spec describes one initialization or batch write.
type Spec struct {
	Mode       Mode
	MinActive  *int64 // lower bound of active candidates; nil uses the default: supplier=1, lottery=0
	Replace    bool   // true replaces everything (stock is reset), false merges (disabled state and remaining stock are kept)
	Candidates []Candidate
}

// Result is the outcome of Pick or Draw.
type Result struct {
	SelectorID  string
	CandidateID string
	Weight      int64 // returned by Pick
	RemainStock int64 // returned by Draw; -1 means unlimited
	Version     int64 // weight table version, incremented on every rebuild
	Replayed    bool  // Draw hit an idempotency key; no stock was deducted this time
}

type CandidateState struct {
	Candidate
	Enabled bool
}

// Snapshot is the full state of a selector, for admin consoles and local fallback.
type Snapshot struct {
	Mode        Mode
	Enabled     bool
	Version     int64
	TotalWeight int64
	ActiveCount int64
	MinActive   int64
	Candidates  []CandidateState // sorted by ID ascending
}

// Picker and Drawer are the hot-path interfaces; Admin is the maintenance interface.
// Business code depends only on the first two; inject Admin separately to isolate privileges.
type Picker interface {
	Pick(ctx context.Context, selectorID string, opts ...Option) (*Result, error)
}

type Drawer interface {
	Draw(ctx context.Context, selectorID string, opts ...Option) (*Result, error)
}

type Admin interface {
	Init(ctx context.Context, selectorID string, spec Spec) error
	Add(ctx context.Context, selectorID string, c Candidate) error
	SetWeight(ctx context.Context, selectorID, candidateID string, weight int64) error
	Disable(ctx context.Context, selectorID, candidateID string) error
	Enable(ctx context.Context, selectorID, candidateID string) error
	Remove(ctx context.Context, selectorID, candidateID string) error
	AddStock(ctx context.Context, selectorID, candidateID string, delta int64) error
	SetSelectorEnabled(ctx context.Context, selectorID string, enabled bool) error
	Rebuild(ctx context.Context, selectorID string) error
	Describe(ctx context.Context, selectorID string) (*Snapshot, error)
	Destroy(ctx context.Context, selectorID string) error
}

type Selector interface {
	Picker
	Drawer
	Admin
}

// config is fixed at construction time. KeyPrefix and Namespace decide where keys live,
// so they are instance-level only and cannot be overridden per call.
type config struct {
	KeyPrefix     string        // optional prefix such as "pay:prod:"; empty means none
	Namespace     string        // defaults to "default"
	MaxCandidates int           // defaults to 1000
	MaxWeight     int64         // defaults to 1_000_000
	IdemTTL       time.Duration // how long idempotency records are kept, defaults to 24h
	Seed          func() int64  // seed source, defaults to crypto/rand; must return a value in [1, 2^31-2]
	Observer      func(ctx context.Context, e Event)
}

// ConfigOption configures a Selector at construction time.
type ConfigOption func(*config)

func WithKeyPrefix(prefix string) ConfigOption {
	return func(c *config) { c.KeyPrefix = prefix }
}

func WithNamespace(ns string) ConfigOption {
	return func(c *config) { c.Namespace = ns }
}

func WithMaxCandidates(n int) ConfigOption {
	return func(c *config) { c.MaxCandidates = n }
}

func WithMaxWeight(w int64) ConfigOption {
	return func(c *config) { c.MaxWeight = w }
}

func WithIdemTTL(ttl time.Duration) ConfigOption {
	return func(c *config) { c.IdemTTL = ttl }
}

func WithSeedFunc(fn func() int64) ConfigOption {
	return func(c *config) { c.Seed = fn }
}

func WithObserver(fn func(ctx context.Context, e Event)) ConfigOption {
	return func(c *config) { c.Observer = fn }
}

// Event is reported once after every script execution, for metrics, logs, tracing and auditing.
type Event struct {
	Script     string
	SelectorID string
	Cost       time.Duration
	Err        error
}

type Option func(*options)

type options struct {
	seed    int64
	idemKey string
	exclude []string
}

// WithSeed fixes the random seed. For tests and reproduction only; never pass values taken from external requests.
func WithSeed(seed int64) Option { return func(o *options) { o.seed = seed } }

// WithIdempotencyKey makes repeated Draw calls with the same key return the first result (use an order or request number).
func WithIdempotencyKey(key string) Option { return func(o *options) { o.idemKey = key } }

// WithExclude makes this Pick skip the given suppliers, for retrying with another supplier after a failed call.
func WithExclude(ids ...string) Option { return func(o *options) { o.exclude = ids } }

var (
	ErrInvalidArgument     = errors.New("selector: invalid argument")
	ErrSelectorNotFound    = errors.New("selector: selector not found")
	ErrSelectorDisabled    = errors.New("selector: selector disabled")
	ErrModeMismatch        = errors.New("selector: mode mismatch")
	ErrNoAvailable         = errors.New("selector: no available candidate")
	ErrLastActiveProtected = errors.New("selector: cannot disable or remove the last active candidate")
	ErrCandidateNotFound   = errors.New("selector: candidate not found")
	ErrTooManyCandidates   = errors.New("selector: too many candidates")
	ErrInvalidWeight       = errors.New("selector: invalid weight")
	ErrInvalidStock        = errors.New("selector: invalid stock")
	ErrStockUnlimited      = errors.New("selector: stock is unlimited")
	ErrInconsistent        = errors.New("selector: inconsistent state")
	ErrBadReply            = errors.New("selector: unexpected script reply")
)

var luaErrors = map[string]error{
	"ERR_SELECTOR_NOT_FOUND":    ErrSelectorNotFound,
	"ERR_SELECTOR_DISABLED":     ErrSelectorDisabled,
	"ERR_MODE_MISMATCH":         ErrModeMismatch,
	"ERR_NO_AVAILABLE":          ErrNoAvailable,
	"ERR_LAST_ACTIVE_PROTECTED": ErrLastActiveProtected,
	"ERR_CANDIDATE_NOT_FOUND":   ErrCandidateNotFound,
	"ERR_TOO_MANY_CANDIDATES":   ErrTooManyCandidates,
	"ERR_INVALID_ARGUMENT":      ErrInvalidArgument,
	"ERR_INVALID_WEIGHT":        ErrInvalidWeight,
	"ERR_INVALID_STOCK":         ErrInvalidStock,
	"ERR_STOCK_UNLIMITED":       ErrStockUnlimited,
	"ERR_INCONSISTENT":          ErrInconsistent,
}
```

### 4.2 Method Semantics

| Method | Semantics |
| --- | --- |
| `Pick` | Supplier mode. Returns one active supplier chosen at random by weight. `WithExclude` skips suppliers that already failed in this attempt |
| `Draw` | Lottery mode. Draws by weight; a limited prize also deducts one unit of stock. `WithIdempotencyKey` makes a repeated request return the first result |
| `Init` | Creates a selector or writes candidates in bulk. `Replace=true` replaces everything and resets stock; `false` merges, keeping the disabled state and remaining stock of existing candidates |
| `Add` | Adds a candidate. If it already exists, its weight and attributes are overwritten and it is re-enabled. Safe to call repeatedly |
| `SetWeight` | Changes the weight only; does not change the enabled state |
| `Disable` | Temporarily takes a candidate out of the pool and keeps its configuration. Subject to last-active protection |
| `Enable` | Puts a disabled candidate back into the pool |
| `Remove` | Deletes a candidate and its configuration. Subject to last-active protection. A disabled candidate can be removed directly |
| `AddStock` | Adjusts the stock of a limited prize. When stock goes from 0 to a positive value, the prize returns to the pool automatically |
| `SetSelectorEnabled` | Master switch for the whole selector, for emergency use |
| `Rebuild` | Regenerates the cumulative weight table from the current configuration without changing it. Use it after `ErrInconsistent` appears and the source data has been checked |
| `Describe` | Returns a full snapshot, for admin consoles and local fallback |
| `Destroy` | Deletes all keys of the selector, for cleanup after an activity ends |

Removing a candidate and adding it back has two paths. To keep the configuration, use `Disable` followed by `Enable`. For a candidate that has been `Remove`d, register it again with `Add`.

### 4.3 Repository Layout

```text
redis-selector/
  go.mod                      core module, zero third-party dependencies
  selector/
    types.go                  types, interfaces, configuration, errors
    core.go                   keys, validation, script loading and execution, reply parsing
    ops.go                    Pick, Draw and maintenance methods
    *_test.go                 unit and integration tests (integration tests need REDIS_ADDR)
    scripts/
      lib.lua                 shared prelude
      lottery_draw.lua
      supplier_pick.lua
      selector_mutate.lua
      selector_init.lua
      selector_query.lua
      selector_destroy.lua
  adapter/
    goredisv9/  go.mod        each adapter is its own module
    goredisv8/  go.mod
    gozero/     go.mod
    goframe/    go.mod
  example/
    go.mod
    supplier/main.go
    lottery/main.go
    adapter_test.go           end-to-end tests for the go-redis/v9 adapter
  .github/workflows/ci.yml    CI with a Redis service container
  docs/functional-design.md
  README.md
  README.zh-CN.md
  README.ja.md
  README.ko.md
```

Each adapter is a separate Go module. A project that uses go-redis does not pull go-zero or GoFrame into its dependency tree by adding this component.


---

## 5. Lua Scripts

### 5.1 Conventions

- Script files are static and embedded into the binary with `embed`. All dynamic values enter through `KEYS` and `ARGV`; scripts never build key names.
- At load time Go prepends `lib.lua` to every business script, so the shared logic exists in only one copy.
- Replies are always flat arrays of strings and integers. Success is `{1, ...}`; failure is `{0, 'ERR_XXX'}`. Business errors do not use `redis.error_reply`, so adapters need not tell business errors apart from network errors. Structured data travels as a JSON string, because GoFrame converts array replies to `[]string` and flattens nested arrays.
- Rebuilding sorts candidates by ID before accumulating, so the primary, the replicas and AOF replay all produce the same result.

### 5.2 Random Numbers

`math.random` is unreliable in Redis's Lua environment. In earlier versions it reset the seed to a fixed value before every script run, and the behavior differs between versions. The component does not use it. It takes a two-level approach:

1. **Default**: Go generates a seed in `[1, 2^31-2]` with `crypto/rand` and passes it in `ARGV[1]`. Each call gets a new seed, so outside parties cannot predict the result.
2. **Fallback**: when the seed is missing or not greater than 0, the script builds one from the seconds and microseconds of `TIME`. Only supplier selection may use this path. A lottery draw fails with an error if the random source is unavailable.

Inside the script the seed passes through the MINSTD linear congruential generator: `state = state * 48271 mod (2^31 - 1)`. Lua 5.1 numbers are doubles, which are exact only up to 2^53. The largest product this generator computes is about 1.04 * 10^14, so no precision is lost. The seed is iterated three times before use, so adjacent seeds (for example consecutive microseconds) still give well spread results. In a test using the consecutive integers 1 to 20000 as seeds, a 70/30 weighting produced a hit rate of 0.7000.

On the fallback path the script calls `TIME` before it writes. Since Redis 5.0 scripts replicate by effects by default, so this is safe. Versions 3.2 to 4.x need `redis.replicate_commands()` at the start of the script. This component does not support those versions.

### 5.3 lib.lua

```lua
-- lib.lua: shared prelude. Go prepends it to every script at load time so that all scripts use the same implementation.
local RS_M = 2147483647 -- 2^31-1 (prime)
local RS_A = 48271      -- MINSTD multiplier; RS_A * RS_M < 2^53, so Lua doubles lose no precision

-- Normalize the seed. A missing or non-positive seed falls back to TIME (seconds * 10^6 + microseconds).
local function rs_seed(raw)
  local seed = tonumber(raw) or 0
  if seed <= 0 then
    local t = redis.call('TIME')
    seed = tonumber(t[1]) * 1000000 + tonumber(t[2])
  end
  local state = seed % (RS_M - 1) + 1 -- map into [1, M-1], away from the fixed point 0
  for _ = 1, 3 do state = (state * RS_A) % RS_M end -- warm up to spread adjacent seeds apart
  return state
end

-- Return an integer in [1, n] and the advanced state.
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
```

### 5.4 lottery_draw.lua

Flow: first check the idempotency record; then validate the selector; select a prize by weight; for a limited prize, deduct one unit of stock, and rebuild the cumulative weights when the stock reaches 0; finally write the idempotency record. The whole sequence runs once on Redis's single thread, so concurrent requests never interleave, and stock never goes negative.

```lua
-- lottery_draw.lua: weighted draw; limited prizes have their stock deducted in the same script.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, [6 idem]
-- ARGV: 1 seed, 2 idem_ttl_sec
-- Returns: {1, candidate_id, remain_stock (-1 = unlimited), version, replayed (0/1)} or {0, err}
local k_meta, k_cum, k_weight, k_stock, k_enabled, k_idem =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]

if k_idem and (not tonumber(ARGV[2]) or not string.match(ARGV[2], '^[1-9]%d*$') or tonumber(ARGV[2]) > 9223372036) then
  return {0, 'ERR_INVALID_ARGUMENT'}
end

-- Idempotency: a repeated request with the same key returns the first result and deducts nothing.
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
  return {0, 'ERR_INVALID_STOCK'} -- a missing stock field must not be treated as unlimited
end

-- Self-heal: meta and cum disagree, or a zero-stock prize is still in cum. Rebuild and let Go retry once.
if (not cid) or stock == 0 then
  rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
  return {0, 'ERR_INCONSISTENT'}
end

local remain, version = -1, meta.version
if stock > 0 then
  remain = redis.call('HINCRBY', k_stock, cid, -1)
  if remain == 0 then
    -- Sold out: the whole prefix sum must be rebuilt; a bare ZREM would let the next prize absorb this weight range.
    local _, _, v = rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
    version = v
  end
end

if k_idem then
  redis.call('SET', k_idem, cjson.encode({cid, remain, version}), 'EX', tonumber(ARGV[2]) or 86400)
end
return {1, cid, remain, version, 0}
```

When every prize has sold out, the script returns `ERR_NO_AVAILABLE`. A common setup is to add an unlimited "thanks for participating" prize, so that every draw returns a result.

### 5.5 supplier_pick.lua

This script writes no keys. The normal path makes one `HMGET`, one `ZRANGEBYSCORE` and one `HGET`.

```lua
-- supplier_pick.lua: pick one supplier at random by weight. Read-only; writes no key.
-- KEYS: 1 meta, 2 cum, 3 weight
-- ARGV: 1 seed, 2..n supplier IDs to exclude this time (optional, for retrying with another supplier after a failure)
-- Returns: {1, supplier_id, weight, version} or {0, err}
local meta, err = rs_meta(KEYS[1], 'supplier')
if not meta then return {0, err} end
if meta.enabled == '0' then return {0, 'ERR_SELECTOR_DISABLED'} end
if meta.total <= 0 then return {0, 'ERR_NO_AVAILABLE'} end

local state = rs_seed(ARGV[1])
local sid
if #ARGV <= 1 then
  -- Normal path: binary search by score on the ZSET, O(log N).
  local r = rs_rand(state, meta.total)
  sid = redis.call('ZRANGEBYSCORE', KEYS[2], r, '+inf', 'LIMIT', 0, 1)[1]
else
  -- Exclusion path: recover each weight from adjacent score differences, drop the excluded ones, scan linearly, O(N).
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
```

### 5.6 selector_mutate.lua

All single-candidate maintenance operations are here, shared by both modes. The last-active protection sits in the `disable` and `remove` branches. It checks the real number of members in `cum` and makes the check and the change within one script. If two instances take the last two suppliers offline at the same time, only one of them succeeds.

```lua
-- selector_mutate.lua: add, update, remove, enable or disable one candidate, plus the selector master switch. Shared by both modes.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
-- ARGV: 1 op, 2 id, 3 weight, 4 stock or stock delta, 5 item_json, 6 max_candidates
-- Returns: {1, active_count, total_weight, version} or {0, err}
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
```

### 5.7 selector_init.lua

```lua
-- selector_init.lua: create a selector, or write candidates in bulk.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
-- ARGV: 1 mode, 2 min_active, 3 replace (1 = replace all, 0 = merge), 4 max_candidates, then 4 values per candidate: id, weight, stock, item_json
-- Returns: {1, active_count, total_weight, version} or {0, err}
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
  redis.call('HSETNX', k_enabled, id, '1')
  if mode == 'lottery' then
    redis.call('HSETNX', k_stock, id, ARGV[i + 2])
  end
end

redis.call('HSET', k_meta, 'schema', '1', 'mode', mode, 'min_active', ARGV[2])
redis.call('HSETNX', k_meta, 'enabled', '1')
local active, total, version = rs_rebuild(k_meta, k_cum, k_weight, k_stock, k_enabled)
return {1, active, total, version}
```

### 5.8 selector_query.lua and selector_destroy.lua

```lua
-- selector_query.lua: read the full state of a selector for admin consoles and local snapshots. Read-only.
-- KEYS: 1 meta, 2 weight, 3 stock, 4 enabled, 5 items
-- Returns: {1, json} or {0, err}. Structured data travels as a JSON string because some clients flatten nested arrays.
local meta = rs_hmap(KEYS[1])
if not meta.mode then return {0, 'ERR_SELECTOR_NOT_FOUND'} end
return {1, cjson.encode({
  meta = meta,
  weight = rs_hmap(KEYS[2]),
  stock = rs_hmap(KEYS[3]),
  enabled = rs_hmap(KEYS[4]),
  items = rs_hmap(KEYS[5]),
})}
```

```lua
-- selector_destroy.lua: delete all keys of a selector. Idempotency records expire by their own TTL.
-- KEYS: 1 meta, 2 cum, 3 weight, 4 stock, 5 enabled, 6 items
return {1, redis.call('DEL', unpack(KEYS))}
```

---

## 6. Go Core Implementation

### 6.1 Script Loading and Execution, Key Generation, Seeds

Key points:

- **No dependence on SCRIPT LOAD.** The script's SHA1 is computed locally. A call sends `EVALSHA` first. If Redis replies `NOSCRIPT`, the call sends `EVAL`, and Redis caches the script on that node as it runs. Later calls use `EVALSHA` again. After a Cluster resize, a primary/replica failover or `SCRIPT FLUSH`, this path recovers automatically.
- **`NOSCRIPT` is detected by matching the error text.** All four client libraries pass the Redis error text through unchanged.
- **Reply normalization.** go-redis and go-zero return `[]any`, with integers as `int64`; GoFrame returns `[]string`. `toSlice`, `toInt` and `toString` handle all of these.
- **IDs are validated before keys are built.** A failed check returns `ErrInvalidArgument` directly, without contacting Redis.

```go
package selector

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed scripts/*.lua
var scriptFS embed.FS

var scriptNames = []string{
	"lottery_draw", "supplier_pick", "selector_mutate",
	"selector_init", "selector_query", "selector_destroy",
}

type script struct{ src, sha string }

// loadScripts prepends lib.lua to every script and computes its SHA1 locally.
// A local SHA removes the need for SCRIPT LOAD, which reaches only one node in Cluster and is unreliable for warm-up.
func loadScripts() (map[string]script, error) {
	lib, err := scriptFS.ReadFile("scripts/lib.lua")
	if err != nil {
		return nil, err
	}
	out := make(map[string]script, len(scriptNames))
	for _, name := range scriptNames {
		body, err := scriptFS.ReadFile("scripts/" + name + ".lua")
		if err != nil {
			return nil, err
		}
		src := string(lib) + "\n" + string(body)
		sum := sha1.Sum([]byte(src))
		out[name] = script{src: src, sha: hex.EncodeToString(sum[:])}
	}
	return out, nil
}

var (
	idPattern     = regexp.MustCompile("^[A-Za-z0-9_.-]{1,64}$")
	prefixPattern = regexp.MustCompile("^[A-Za-z0-9_.:-]{0,64}$")
)

const (
	seedMax      = 2147483646 // seeds fall in [1, 2^31-2]
	maxItemBytes = 4096
	maxStock     = 1 << 53 // Lua numbers are doubles; stock above 2^53 cannot be compared exactly
)

// checkID rejects braces, colons, whitespace and control characters: braces change the hash tag, and colons let different (namespace, id) pairs build the same key.
func checkID(kind, v string) error {
	if !idPattern.MatchString(v) {
		return fmt.Errorf("%w: %s %q must match %s", ErrInvalidArgument, kind, v, idPattern)
	}
	return nil
}

type keySet struct {
	id, base                                 string
	meta, cum, weight, stock, enabled, items string
}

func (k keySet) six() []string {
	return []string{k.meta, k.cum, k.weight, k.stock, k.enabled, k.items}
}

func (k keySet) idem(key string) string { return k.base + ":idem:" + key }

type selector struct {
	cli     RedisClient
	cfg     config
	scripts map[string]script
}

func New(cli RedisClient, opts ...ConfigOption) (Selector, error) {
	if cli == nil {
		return nil, fmt.Errorf("%w: nil redis client", ErrInvalidArgument)
	}
	var cfg config
	for _, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: nil config option", ErrInvalidArgument)
		}
		opt(&cfg)
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if err := checkID("namespace", cfg.Namespace); err != nil {
		return nil, err
	}
	if !prefixPattern.MatchString(cfg.KeyPrefix) {
		return nil, fmt.Errorf("%w: key prefix %q must match %s", ErrInvalidArgument, cfg.KeyPrefix, prefixPattern)
	}
	if cfg.KeyPrefix != "" && !strings.HasSuffix(cfg.KeyPrefix, ":") {
		cfg.KeyPrefix += ":"
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 1000
	}
	if cfg.MaxWeight <= 0 {
		cfg.MaxWeight = 1_000_000
	}
	if cfg.MaxCandidates > 1000 || cfg.MaxWeight > 1_000_000 {
		return nil, fmt.Errorf("%w: exceeds hard limit", ErrInvalidArgument)
	}
	if cfg.IdemTTL != 0 && (cfg.IdemTTL < time.Second || cfg.IdemTTL%time.Second != 0) {
		return nil, fmt.Errorf("%w: TTL must be positive whole seconds", ErrInvalidArgument)
	}
	if cfg.IdemTTL == 0 {
		cfg.IdemTTL = 24 * time.Hour
	}
	if cfg.Seed == nil {
		cfg.Seed = cryptoSeed
	}
	scripts, err := loadScripts()
	if err != nil {
		return nil, err
	}
	return &selector{cli: cli, cfg: cfg, scripts: scripts}, nil
}

// cryptoSeed draws a seed from a cryptographically secure source so that lottery results cannot be predicted.
// It returns 0 on failure, which makes the script fall back to TIME as the seed.
func cryptoSeed() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(seedMax))
	if err != nil {
		return 0
	}
	return n.Int64() + 1
}

func (s *selector) seed(o options) int64 {
	if o.seed > 0 {
		return o.seed%seedMax + 1
	}
	return s.cfg.Seed()
}

// keys builds [KeyPrefix]{rs:<namespace>:<selector_id>}:<suffix>.
// The prefix sits left of the hash tag and does not affect the slot; all keys of one selector share one slot.
func (s *selector) keys(selectorID string) (keySet, error) {
	if err := checkID("selector id", selectorID); err != nil {
		return keySet{}, err
	}
	base := s.cfg.KeyPrefix + "{rs:" + s.cfg.Namespace + ":" + selectorID + "}"
	return keySet{
		id: selectorID, base: base,
		meta: base + ":meta", cum: base + ":cum", weight: base + ":weight",
		stock: base + ":stock", enabled: base + ":enabled", items: base + ":items",
	}, nil
}

// run tries EVALSHA first and, on NOSCRIPT, runs EVAL once with the source (which also caches the script on that node).
// On success it returns the reply without the leading status flag.
func (s *selector) run(ctx context.Context, name string, k keySet, keys []string, args ...any) (out []any, err error) {
	if s.cfg.Observer != nil {
		start := time.Now()
		defer func() {
			s.cfg.Observer(ctx, Event{Script: name, SelectorID: k.id, Cost: time.Since(start), Err: err})
		}()
	}
	sc := s.scripts[name]
	res, err := s.cli.EvalSha(ctx, sc.sha, keys, args...)
	if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
		res, err = s.cli.Eval(ctx, sc.src, keys, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("selector: run %s: %w", name, err)
	}
	arr := toSlice(res)
	if len(arr) < 2 {
		return nil, fmt.Errorf("%w: %s returned %v", ErrBadReply, name, res)
	}
	if toInt(arr[0]) != 1 {
		code := toString(arr[1])
		if e, ok := luaErrors[code]; ok {
			return nil, e
		}
		return nil, fmt.Errorf("%w: %s", ErrBadReply, code)
	}
	return arr[1:], nil
}

// Clients represent array replies differently: go-redis and go-zero use []any (integers as int64), GoFrame uses []string.
func toSlice(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return nil
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return fmt.Sprint(v)
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	}
	n, _ := strconv.ParseInt(toString(v), 10, 64)
	return n
}
```

### 6.2 Pick, Draw and Maintenance Methods

```go
package selector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

func buildOptions(opts []Option) (options, error) {
	var o options
	for _, fn := range opts {
		if fn == nil {
			return o, fmt.Errorf("%w: nil option", ErrInvalidArgument)
		}
		fn(&o)
	}
	if o.idemKey != "" {
		if err := checkID("idempotency key", o.idemKey); err != nil {
			return o, err
		}
	}
	for _, id := range o.exclude {
		if err := checkID("excluded id", id); err != nil {
			return o, err
		}
	}
	return o, nil
}

// Draw runs a lottery draw. Stock is deducted inside the script; with an idempotency key, retrying after a timeout never deducts twice.
func (s *selector) Draw(ctx context.Context, selectorID string, opts ...Option) (*Result, error) {
	o, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}
	k, err := s.keys(selectorID)
	if err != nil {
		return nil, err
	}
	keys := k.six()[:5]
	if o.idemKey != "" {
		keys = append(keys, k.idem(o.idemKey))
	}
	var out []any
	for attempt := 0; attempt < 2; attempt++ {
		seed := s.seed(o)
		if seed <= 0 {
			return nil, fmt.Errorf("%w: lottery seed unavailable", ErrInvalidArgument)
		}
		out, err = s.run(ctx, "lottery_draw", k, keys, seed, int64(s.cfg.IdemTTL/time.Second))
		if !errors.Is(err, ErrInconsistent) {
			break // the script already rebuilt the index when it found the inconsistency; one retry is enough
		}
	}
	if err != nil {
		return nil, err
	}
	if len(out) < 4 {
		return nil, ErrBadReply
	}
	return &Result{
		SelectorID:  selectorID,
		CandidateID: toString(out[0]),
		RemainStock: toInt(out[1]),
		Version:     toInt(out[2]),
		Replayed:    toInt(out[3]) == 1,
	}, nil
}

// Pick selects one supplier at random by weight. It is read-only; an inconsistent index is returned as an error for the admin side to investigate.
func (s *selector) Pick(ctx context.Context, selectorID string, opts ...Option) (*Result, error) {
	o, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}
	k, err := s.keys(selectorID)
	if err != nil {
		return nil, err
	}
	keys := []string{k.meta, k.cum, k.weight}
	if len(o.exclude) > s.cfg.MaxCandidates {
		return nil, fmt.Errorf("%w: too many excluded ids", ErrInvalidArgument)
	}
	args := make([]any, 0, 1+len(o.exclude))
	args = append(args, s.seed(o))
	for _, id := range o.exclude {
		args = append(args, id)
	}
	out, err := s.run(ctx, "supplier_pick", k, keys, args...)
	if err != nil {
		return nil, err
	}
	if len(out) < 3 {
		return nil, ErrBadReply
	}
	return &Result{
		SelectorID:  selectorID,
		CandidateID: toString(out[0]),
		Weight:      toInt(out[1]),
		RemainStock: -1,
		Version:     toInt(out[2]),
	}, nil
}

func (s *selector) checkCandidate(c Candidate, mode Mode) (item string, err error) {
	if err = checkID("candidate id", c.ID); err != nil {
		return "", err
	}
	if c.Weight < 1 || c.Weight > s.cfg.MaxWeight {
		return "", fmt.Errorf("%w: %s weight %d not in [1, %d]", ErrInvalidWeight, c.ID, c.Weight, s.cfg.MaxWeight)
	}
	if mode == ModeLottery && (c.Stock < -1 || c.Stock > maxStock) {
		return "", fmt.Errorf("%w: %s stock %d", ErrInvalidStock, c.ID, c.Stock)
	}
	b, err := json.Marshal(map[string]any{"name": c.Name, "ext": c.Ext})
	if err != nil {
		return "", err
	}
	if len(b) > maxItemBytes {
		return "", fmt.Errorf("%w: %s name and ext exceed %d bytes", ErrInvalidArgument, c.ID, maxItemBytes)
	}
	return string(b), nil
}

// Init creates a selector or writes candidates in bulk. Writing and rebuilding happen in one script, so other instances never see a half-written state.
func (s *selector) Init(ctx context.Context, selectorID string, spec Spec) error {
	k, err := s.keys(selectorID)
	if err != nil {
		return err
	}
	if spec.Mode != ModeLottery && spec.Mode != ModeSupplier {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidArgument, spec.Mode)
	}
	if n := len(spec.Candidates); n == 0 || n > s.cfg.MaxCandidates {
		return fmt.Errorf("%w: need 1..%d candidates, got %d", ErrInvalidArgument, s.cfg.MaxCandidates, n)
	}
	minActive := int64(0)
	if spec.Mode == ModeSupplier {
		minActive = 1
	}
	if spec.MinActive != nil {
		minActive = *spec.MinActive
	}
	// When merging, existing candidates also count as active, so the script decides whether the lower bound is met.
	if (spec.Mode == ModeSupplier && minActive < 1) || minActive < 0 ||
		(spec.Replace && minActive > int64(len(spec.Candidates))) {
		return fmt.Errorf("%w: min active %d", ErrInvalidArgument, minActive)
	}
	replace := 0
	if spec.Replace {
		replace = 1
	}
	args := []any{string(spec.Mode), minActive, replace, s.cfg.MaxCandidates}
	seen := make(map[string]bool, len(spec.Candidates))
	for _, c := range spec.Candidates {
		item, err := s.checkCandidate(c, spec.Mode)
		if err != nil {
			return err
		}
		if seen[c.ID] {
			return fmt.Errorf("%w: duplicate candidate %s", ErrInvalidArgument, c.ID)
		}
		seen[c.ID] = true
		stock := int64(-1)
		if spec.Mode == ModeLottery {
			stock = c.Stock
		}
		args = append(args, c.ID, c.Weight, stock, item)
	}
	_, err = s.run(ctx, "selector_init", k, k.six(), args...)
	return err
}

func (s *selector) mutate(ctx context.Context, selectorID, op, candidateID string, weight, stock int64, item string) error {
	k, err := s.keys(selectorID)
	if err != nil {
		return err
	}
	if candidateID != "" {
		if err := checkID("candidate id", candidateID); err != nil {
			return err
		}
	}
	_, err = s.run(ctx, "selector_mutate", k, k.six(), op, candidateID, weight, stock, item, s.cfg.MaxCandidates)
	return err
}

// Add is an idempotent upsert: an existing candidate gets its weight and attributes overwritten and is re-enabled. Initial stock is set only when a prize is first registered;
// Stock is ignored in supplier mode.
func (s *selector) Add(ctx context.Context, selectorID string, c Candidate) error {
	item, err := s.checkCandidate(c, ModeSupplier)
	if err != nil {
		return err
	}
	if c.Stock > maxStock {
		return fmt.Errorf("%w: %s stock %d", ErrInvalidStock, c.ID, c.Stock)
	}
	return s.mutate(ctx, selectorID, "add", c.ID, c.Weight, c.Stock, item)
}

func (s *selector) SetWeight(ctx context.Context, selectorID, candidateID string, weight int64) error {
	if weight < 1 || weight > s.cfg.MaxWeight {
		return fmt.Errorf("%w: %d not in [1, %d]", ErrInvalidWeight, weight, s.cfg.MaxWeight)
	}
	return s.mutate(ctx, selectorID, "set_weight", candidateID, weight, 0, "")
}

// Disable temporarily takes a candidate out of the pool and keeps its configuration; Enable puts it back.
// If it is the last active candidate, ErrLastActiveProtected is returned and nothing changes.
func (s *selector) Disable(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "disable", candidateID, 0, 0, "")
}

func (s *selector) Enable(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "enable", candidateID, 0, 0, "")
}

// Remove deletes a candidate and its configuration; only Add can bring it back. Last-active protection applies as in Disable.
func (s *selector) Remove(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "remove", candidateID, 0, 0, "")
}

// AddStock adjusts the stock of a limited prize. delta may be negative; the result cannot go below 0.
func (s *selector) AddStock(ctx context.Context, selectorID, candidateID string, delta int64) error {
	if delta > maxStock || delta < -maxStock {
		return fmt.Errorf("%w: delta %d", ErrInvalidStock, delta)
	}
	return s.mutate(ctx, selectorID, "incr_stock", candidateID, 0, delta, "")
}

// Rebuild regenerates the cumulative weight table from weight, enabled and stock without changing any configuration.
// Use it to repair the index after ErrInconsistent, once the source data has been checked.
func (s *selector) Rebuild(ctx context.Context, selectorID string) error {
	return s.mutate(ctx, selectorID, "rebuild", "", 0, 0, "")
}

// SetSelectorEnabled is the master switch of a selector; when off, Pick and Draw return ErrSelectorDisabled.
func (s *selector) SetSelectorEnabled(ctx context.Context, selectorID string, enabled bool) error {
	op := "selector_disable"
	if enabled {
		op = "selector_enable"
	}
	return s.mutate(ctx, selectorID, op, "", 0, 0, "")
}

func (s *selector) Destroy(ctx context.Context, selectorID string) error {
	k, err := s.keys(selectorID)
	if err != nil {
		return err
	}
	_, err = s.run(ctx, "selector_destroy", k, k.six())
	return err
}

func (s *selector) Describe(ctx context.Context, selectorID string) (*Snapshot, error) {
	k, err := s.keys(selectorID)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, "selector_query", k, []string{k.meta, k.weight, k.stock, k.enabled, k.items})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Meta, Weight, Stock, Enabled, Items map[string]string
	}
	if err := json.Unmarshal([]byte(toString(out[0])), &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadReply, err)
	}
	num := func(v string) int64 { n, _ := strconv.ParseInt(v, 10, 64); return n }
	snap := &Snapshot{
		Mode:        Mode(raw.Meta["mode"]),
		Enabled:     raw.Meta["enabled"] != "0",
		Version:     num(raw.Meta["version"]),
		TotalWeight: num(raw.Meta["total_weight"]),
		ActiveCount: num(raw.Meta["active_count"]),
		MinActive:   num(raw.Meta["min_active"]),
	}
	for id, w := range raw.Weight {
		c := CandidateState{Enabled: raw.Enabled[id] == "1"} // same condition the rebuild function uses
		c.ID, c.Weight, c.Stock = id, num(w), -1
		if v, ok := raw.Stock[id]; ok {
			c.Stock = num(v)
		}
		var item struct {
			Name string
			Ext  map[string]string
		}
		_ = json.Unmarshal([]byte(raw.Items[id]), &item)
		c.Name, c.Ext = item.Name, item.Ext
		snap.Candidates = append(snap.Candidates, c)
	}
	sort.Slice(snap.Candidates, func(i, j int) bool { return snap.Candidates[i].ID < snap.Candidates[j].ID })
	return snap, nil
}
```


---

## 7. Redis Client Adapters

Adapters forward only `Eval` and `EvalSha`. Whether the client is standalone or cluster is decided by the client you pass in, so neither the adapter nor the core package needs to distinguish them. In cluster mode the client computes the slot from the `keys` argument and routes the call to the right node. All keys in one call of this component are in the same slot.

### 7.1 go-redis/v9

```go
// Package goredisv9 adapts github.com/redis/go-redis/v9.
package goredisv9

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Adapter accepts any client that implements redis.Scripter:
// *redis.Client (standalone), *redis.ClusterClient (cluster), redis.UniversalClient, *redis.Ring.
type Adapter struct{ c redis.Scripter }

func New(c redis.Scripter) *Adapter { return &Adapter{c: c} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.c.Eval(ctx, script, keys, args...).Result()
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.c.EvalSha(ctx, sha1, keys, args...).Result()
}
```

### 7.2 go-redis/v8

```go
// Package goredisv8 adapts github.com/go-redis/redis/v8. It is the same as the v9 adapter apart from the import path.
package goredisv8

import (
	"context"

	"github.com/go-redis/redis/v8"
)

type Adapter struct{ c redis.Scripter }

func New(c redis.Scripter) *Adapter { return &Adapter{c: c} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.c.Eval(ctx, script, keys, args...).Result()
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.c.EvalSha(ctx, sha1, keys, args...).Result()
}
```

### 7.3 go-zero

```go
// Package gozero adapts github.com/zeromicro/go-zero/core/stores/redis.
package gozero

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Adapter wraps *redis.Redis. Standalone or cluster is decided by RedisConf.Type (node / cluster); the adapter does not care.
type Adapter struct{ r *redis.Redis }

func New(r *redis.Redis) *Adapter { return &Adapter{r: r} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.r.EvalCtx(ctx, script, keys, args...)
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.r.EvalShaCtx(ctx, sha1, keys, args...)
}
```

go-zero's Redis client ships with its own circuit breaker. If the script cache is flushed, the first `NOSCRIPT` reply counts as one failure for each script on each node. That is too few to trip the breaker.

### 7.4 GoFrame Redis Adapter (gredis)

```go
// Package goframe adapts GoFrame's github.com/gogf/gf/v2/database/gredis.
package goframe

import (
	"context"

	"github.com/gogf/gf/v2/database/gredis"
)

// Adapter wraps *gredis.Redis (for example g.Redis()). With several addresses configured, the underlying client is a cluster client.
// gredis turns array replies into []string; the core package accepts that form.
type Adapter struct{ r *gredis.Redis }

func New(r *gredis.Redis) *Adapter { return &Adapter{r: r} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.do(ctx, "EVAL", script, keys, args)
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.do(ctx, "EVALSHA", sha1, keys, args)
}

func (a *Adapter) do(ctx context.Context, cmd, body string, keys []string, args []any) (any, error) {
	params := make([]any, 0, 2+len(keys)+len(args))
	params = append(params, body, len(keys))
	for _, k := range keys {
		params = append(params, k)
	}
	params = append(params, args...)
	v, err := a.r.Do(ctx, cmd, params...)
	if err != nil {
		return nil, err
	}
	return v.Val(), nil
}
```

Any client that can send arbitrary commands can be adapted the same way. Pass the script body or its SHA, the number of keys, the keys and the arguments, in that order, to the client's generic command method.

### 7.5 Compatibility

| Client | Tested version | Standalone | Cluster | Notes |
| --- | --- | --- | --- | --- |
| go-redis/v9 | v9.23.0 | Passed | Passed | Recommended |
| go-redis/v8 | v8.11.5 | Passed | See notes | The v8 cluster client cannot parse the `CLUSTER SLOTS` reply from Redis 7 and later, so it fails while connecting. For a Redis 7+ cluster, use v9, or let v8 connect only to a Redis 6 cluster |
| go-zero | v1.10.3 | Not tested separately | Passed (`Type: cluster`) | `EvalCtx` and `EvalShaCtx` already existed in v1.4.3 |
| GoFrame | v2.9.4 | Passed | Not tested | Requires the `github.com/gogf/gf/contrib/nosql/redis/v2` driver |

The server used for testing was Redis 8.6.3 with Cluster mode enabled. The minimum requirement is Redis 5.0.

---

## 8. Business-Side Code Skeleton

### 8.1 Construction

```go
rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs})

sel, err := selector.New(
    goredisv9.New(rdb),
    selector.WithKeyPrefix("pay:prod"), // optional
    selector.WithNamespace("supplier"),
    selector.WithObserver(metricsObserver), // optional, see section 12
)
if err != nil {
    return err
}

// The business service receives only a Picker; the maintenance service receives Admin.
paySvc := &PaymentService{picker: sel, clients: supplierClients}
opsSvc := &SupplierOps{admin: sel}
```

One instance corresponds to one fixed set of prefix and namespace. When you need several namespaces, create several instances. An instance runs no background goroutines, so creating one is cheap.

### 8.2 Suppliers: Pick, and Switch to Another on Failure

```go
func (s *PaymentService) Pay(ctx context.Context, req *PayReq) error {
    var (
        tried   []string
        lastErr error
    )
    for attempt := 0; attempt < 3; attempt++ {
        res, err := s.picker.Pick(ctx, "payment", selector.WithExclude(tried...))
        if errors.Is(err, selector.ErrNoAvailable) {
            if lastErr == nil { return err }
            break // no supplier left to choose, or all of them have been tried
        }
        if err != nil {
            return err
        }
        client, ok := s.clients[res.CandidateID]
        if !ok { return fmt.Errorf("unknown supplier: %s", res.CandidateID) }
        if lastErr = client.Pay(ctx, req); lastErr == nil {
            return nil
        }
        // The business side returns true only when the request was definitely not accepted.
        // On a timeout or an unknown outcome, query the order instead of retrying.
        if !s.definitelyNotAccepted(lastErr) { return lastErr }
        tried = append(tried, res.CandidateID)
    }
    return fmt.Errorf("all suppliers failed: %w", lastErr)
}
```

`WithExclude` affects this one call only. It does not change any supplier's state in Redis. Retrying with another supplier requires that the downstream call is safe to repeat. Each supplier deduplicates independently, so the same order number cannot stop a duplicate payment across suppliers.

### 8.3 Suppliers: Maintenance

```go
// Take offline: remove from the pool temporarily and keep the configuration.
func (o *SupplierOps) Offline(ctx context.Context, id string) error {
    err := o.admin.Disable(ctx, "payment", id)
    if errors.Is(err, selector.ErrLastActiveProtected) {
        return fmt.Errorf("supplier %s is the last active supplier and cannot go offline: %w", id, err)
    }
    return err
}

// Bring online: return a supplier that was taken offline.
func (o *SupplierOps) Online(ctx context.Context, id string) error {
    return o.admin.Enable(ctx, "payment", id)
}

// Delete: remove it together with its configuration. Last-active protection applies here too.
func (o *SupplierOps) Delete(ctx context.Context, id string) error {
    return o.admin.Remove(ctx, "payment", id)
}

// Register a new supplier, or register a removed one again.
func (o *SupplierOps) Register(ctx context.Context, id, name string, weight int64) error {
    return o.admin.Add(ctx, "payment", selector.Candidate{ID: id, Name: name, Weight: weight})
}

// Adjust the share of traffic.
func (o *SupplierOps) Reweight(ctx context.Context, id string, weight int64) error {
    return o.admin.SetWeight(ctx, "payment", id, weight)
}
```

### 8.4 Last-Active Protection Behavior

| Current state | Operation | Result |
| --- | --- | --- |
| A and B active | `Disable(A)` | Succeeds; afterwards Pick returns only B |
| Only B active, A disabled | `Disable(B)` | Returns `ErrLastActiveProtected`; B keeps serving |
| Only B active, A disabled | `Remove(B)` | Returns `ErrLastActiveProtected`; B keeps serving |
| Only B active, A disabled | `Remove(A)` | Succeeds; A was not in the pool anyway |
| Only B active, A disabled | `Enable(A)`, then `Disable(B)` | Succeeds |
| Only B active | `SetSelectorEnabled(false)` | Succeeds. The master switch is a separate emergency control and is not subject to last-active protection |
| A and B taken offline at the same time | Concurrent `Disable` calls | The first to arrive succeeds; the second returns `ErrLastActiveProtected` |

The lower bound can be adjusted. Setting `Spec.MinActive` to 2 keeps at least two suppliers. Supplier mode rejects 0. When the last supplier fails a call, the business code still returns the error as usual, and later requests can still select that supplier.

### 8.5 Lottery

```go
func (s *LotteryService) Draw(ctx context.Context, userID, requestID string) (*Prize, error) {
    // Eligibility, usage limits and risk checks are done by the business before the component is called.
    res, err := s.drawer.Draw(ctx, "spring2026", selector.WithIdempotencyKey(requestID))
    switch {
    case errors.Is(err, selector.ErrNoAvailable):
        return nil, ErrPrizeSoldOut
    case err != nil:
        // On a timeout or network error the outcome is unknown. Retry with the same requestID;
        // stock will not be deducted twice.
        return nil, err
    }
    // Persist the win. A unique index on (activity, request_id) applies. The database is the final
    // basis for issuing a prize; see 11.3.
    if err := s.repo.SaveWin(ctx, userID, requestID, res.CandidateID); err != nil {
        return nil, err
    }
    return s.prizes[res.CandidateID], nil
}
```

Before an activity starts, call `Init` with `Replace=true` to write the prizes and their stock. During the activity, adjust only with `AddStock`, `SetWeight`, `Disable` and `Enable`. Do not use `Replace=true` again, because it resets stock to its initial values. After the activity ends, call `Destroy`.


---

## 9. Security Design

### 9.1 Risks and Countermeasures

| Risk | Consequence | Countermeasure | Covered in |
| --- | --- | --- | --- |
| Script injection | Arbitrary Redis commands run | Scripts are embedded statically; dynamic values go only through `KEYS` and `ARGV` | 5.1 |
| Key injection, breaking the hash tag | Reads or writes reach another selector, or a cross-slot error in Cluster | Every ID passes an allowlist pattern | 9.2 |
| Lottery results can be predicted or manipulated | An attacker times requests to win big prizes | Seeds come from `crypto/rand` and never from external input | 9.3 |
| Repeated requests deduct stock repeatedly | One request consumes several prizes | Idempotency key | 9.4 |
| Oversized input slows Redis | A script holds the single thread for a long time and affects other workloads on the instance | Limits on count, weight and length | 9.5 |
| Business services perform maintenance without authorization | Accidental takedowns or malicious weight changes | Split interfaces plus Redis ACL | 9.6 |
| Operator mistakes | All suppliers taken offline, or live activity stock reset | Last-active protection, merge writes that keep stock, master switch | 9.7 |
| Sensitive data enters Redis or logs | Supplier secrets leak | `Ext` holds display fields only; logs never print its content | 9.8 |
| Writes lost during Redis failover | Stock may roll back in rare cases | The database is the final basis for issuing prizes | 11.3 |

### 9.2 Key Injection and Hash Tags

Keys are built from the prefix, the namespace and the selector ID. These values may come from configuration or from requests. Without restrictions there are three problems:

- Braces inside an ID change the extent of the hash tag. For example, with the selector ID `a}:{b`, Redis uses only the content of the first pair of braces to compute the slot. The keys of that selector may then share a slot with other keys, or be steered to a chosen slot.
- A colon in the namespace or ID lets `(ns="a:b", id="c")` and `(ns="a", id="b:c")` build the same key.
- Newlines, spaces and other control characters pollute logs and monitoring labels.

The countermeasure is an allowlist. Namespace, selector ID, candidate ID and idempotency key may contain only `[A-Za-z0-9_.-]{1,64}`. The prefix may contain only `[A-Za-z0-9_.:-]{0,64}`. Validation runs before any key is built, so invalid input never reaches Redis. If a business ID contains other characters, for example a Chinese name, use an internal code as the ID and put the name in `Name`.

### 9.3 Random Numbers and Unpredictable Results

- Seeds come from `crypto/rand` by default, one per call. The linear congruential generator in the script only maps a seed onto the weight range. It is not cryptographically secure by itself; security comes from the seed being unpredictable.
- `WithSeed` is for tests and reproducing problems only. Never pass request parameters, user IDs, timestamps or other values that outside parties control or can guess.
- A `TIME`-based fallback seed is guessable, so it is used only when the random source fails. The lottery path rejects a non-positive seed and never falls back to the clock.
- The interface does not return the seed, and logs must not record the mapping between seeds and results.

### 9.4 Idempotency and Replay

After a lottery request times out, the caller does not know whether stock was deducted. With `WithIdempotencyKey(requestID)`, the script stores the first result at `...:idem:<key>` with a TTL (24 hours by default). A later request with the same key returns the first result, and `Replayed` is `true`. Reading the idempotency record, deducting stock and writing the record all happen in the same script.

The idempotency key should be generated or verified on the server side, for example as an order number or a request serial number. If the client can upload any string as the key, a user can draw again just by changing the key. Eligibility and usage limits are business rules and must be checked before `Draw` is called.

### 9.5 Resource Limits

A script runs on Redis's single thread, so oversized input blocks other requests on the same instance. The defaults are therefore: at most 1000 candidates, a single weight of at most 10^6, at most 4096 bytes of attribute JSON, and an exclusion list no longer than the candidate limit. The script checks the candidate count again (`ERR_TOO_MANY_CANDIDATES`), so concurrent `Add` calls from several instances cannot exceed it.

### 9.6 Privilege Separation

The first layer is in code. Business services receive only `Picker` or `Drawer` and have no access to maintenance methods. Maintenance methods live in a separate admin service or console, which handles login, authorization, operator records and approval.

The second layer is Redis ACL (Redis 6 and later). Commands that scripts call are also subject to ACL, so accounts can be split by role into three:

```text
# Supplier business account: read-only
ACL SETUSER rs_pick reset on >password ~pay:prod:{rs:supplier:* -@all
    +evalsha +eval +hmget +hget +zrangebyscore +zrange +time

# Lottery business account: read-only plus stock deduction and rebuild
ACL SETUSER rs_draw reset on >password ~pay:prod:{rs:lottery:* -@all
    +evalsha +eval +hmget +hget +zrangebyscore +zrange +time
    +get +set +hincrby +hgetall +del +zadd +hset

# Admin account
ACL SETUSER rs_admin reset on >password ~pay:prod:{rs:* -@all
    +evalsha +eval +hmget +hget +zrangebyscore +zrange +time
    +get +set +hincrby +hgetall +del +zadd +hset
    +hexists +hlen +hdel +hsetnx +zscore +zcard
```

Each `ACL SETUSER` line is wrapped across several lines here for readability. In practice each one is written on a single line. These command sets were tested: the read-only account can Pick normally, and it is refused when it runs a maintenance script, writes with a plain `HSET`, or touches keys outside the prefix. Keep three points in mind:

- The client library needs its own commands to connect, such as `HELLO`, `PING` and `CLIENT SETINFO`. A cluster client also needs `CLUSTER SLOTS` or `CLUSTER SHARDS`. Write operations must not be retried transparently by the client after a timeout, particularly `Draw` without an idempotency key and `AddStock`. Verify the retry settings for the client in use. Add the connection permissions your client needs, and verify everything in a staging environment before going live.
- Pick does not run maintenance commands. When it finds an inconsistency it raises an alert. The admin side checks the source data and then calls `Rebuild` to repair the index. Do not re-enable a selector that was switched off just to fix the index.
- The examples assign Namespace `supplier` and `lottery` to the two modes. A lottery account can run any write command in its allowlist, so its credentials must never be handed to end users. If callers may only draw, keep the credentials inside a trusted lottery service.

`KeyPrefix` is also useful here. Using different prefixes for different environments and product lines lets ACL key patterns keep them apart.

### 9.7 Protection Against Operator Mistakes

- **Last-active protection**: see 8.4. The lower bound `min_active` is stored in Redis, so every instance and every entry point enforces the same value.
- **Merge writes do not reset stock**: when `Init` runs with `Replace=false`, existing candidates keep their remaining stock and disabled state.
- **Full replacement must be explicit**: `Replace=true` clears and rewrites everything. The admin console should require a second confirmation for this operation and block it while an activity is running.
- **Immutable mode**: calling `Init` on an existing selector with a different mode returns `ErrModeMismatch`, so a supplier selector cannot be mistaken for a lottery and deduct stock.
- **Master switch**: when something goes wrong, `SetSelectorEnabled(false)` stops selection on every instance at once, and the data stays as it is.
- **Audit**: every script run triggers `Observer`. The admin service should record the operator, the operation, the target, the result, and the `version` before and after.

### 9.8 Data and Transport

- `Ext` and `Name` hold display information only. Supplier secrets, certificates and callback signing keys belong in a configuration center or key management system, linked to the candidate by its ID.
- The component does not create Redis connections. The caller configures TLS, passwords, connection pools and timeouts on the client. In production, enable TLS and set a timeout on `ctx`.
- Error messages contain only IDs and numbers, never the content of `Ext`.

---


## 10. Engineering Extension Design

### 10.1 A Unified Candidate Model

Both modes share one data structure and one rebuild function. They differ in two respects: lottery mode has stock, and supplier mode's `min_active` defaults to 1. As a result, maintenance works the same way for both. Prizes can be taken down and put back temporarily, and when suppliers later need quotas (for example, a daily call limit), they can reuse `stock` directly.

To add a new mode, you usually only need to define the mode name and its default `min_active`, write a selection script, and expose the capability through a new small interface. Direct changes to the published interfaces would break external implementations, so avoid them. The maintenance scripts and the rebuild function do not need to change.

### 10.2 New Selection Strategies

A selection strategy is a separate Lua script that shares `lib.lua`. Adding one always follows the same steps: add the file under `scripts/`, register its name in `scriptNames`, and add a method in the core package that calls it. When the script changes, its SHA1 changes too, so old and new processes can run at the same time, each using its own script. Strategies that can be foreseen:

| Strategy | Approach |
| --- | --- |
| Pick N distinct candidates at once (`PickN`) | Reuse the exclusion path and loop inside the script |
| Smooth weighted round-robin | Keep the current weights in a `...:rr` hash next to `meta`, and update it in the script |
| Fixed routing by business key (the same user always goes to the same supplier) | Add a call option that uses a hash of the business key as the seed; no new script is needed |
| Supplier quotas | Supplier mode writes `stock`; the selection script deducts it, with the same logic as a lottery |

### 10.3 Entry Point for Automatic Removal

This version does not include automatic circuit breaking. If you later need "remove a supplier automatically after consecutive failures, and bring it back after a while", build it as a separate module on top of the component. The module records call results, calls `Disable` when a threshold is reached, and calls `Enable` after a probe succeeds. Last-active protection applies naturally, so the last supplier is never removed automatically. The module only needs to handle the single rejection `ErrLastActiveProtected`.

### 10.4 Decorators and Observer

`Picker`, `Drawer` and `Admin` are small interfaces, so decorators can add capabilities without changing the core package:

- **Local fallback** (supplier mode only): a decorator calls `Describe` on a timer and caches the snapshot in memory. When Redis is unavailable, it selects by weight locally from that snapshot. It may keep selecting a supplier that was just removed. It is off by default. Enable it only when the business explicitly accepts stale routing, and set a snapshot expiry; after expiry it returns an error. Lottery cannot use this approach, because stock cannot be guaranteed locally.
- **Rate limiting, tracing and audit logs**: wrap the interface in one more layer.
- **Observer**: register with `WithObserver`. It is called after every script run, with the script name, the selector ID, the duration and the error. Metrics and logs are wired up here, and the core package does not depend on any monitoring library.

### 10.5 Data Structure Evolution

`meta.schema` records the version of the data structure. The evolution rules are:

- Changes that only add fields or keys keep `schema` unchanged. New scripts use default values for missing fields.
- Changes that alter the meaning of an existing key increment `schema` and use new key suffixes. A process that reads an older `schema` returns an explicit error, and the admin side migrates the data by calling `Init`.
- `Describe` combined with `Init` cannot migrate a live lottery without loss. Idempotency keys, TTLs, disabled states and in-flight requests are outside the import contract. To migrate, freeze writes and drain in-flight requests, copy every key and its remaining TTL as of the same point in time, verify stock, state and replay results, and then switch over. After writes resume, never switch back to the old copy directly.

### 10.6 Dependencies and Release

- The core module uses only the standard library. Each adapter is a separate module and follows the major version of its client library.
- The stable public surface is the types, interfaces and error values in `types.go`. Scripts, key suffixes and internal functions are not part of the public contract and may change in minor releases.
- New call options are added as `Option` functions, and new construction settings as `ConfigOption` helpers (`With*`) that work with their zero values. Existing callers need no code changes.

---

## 11. Consistency, Concurrency and Failure Handling

### 11.1 Cluster

- All keys touched by one script call belong to the same selector and share its hash tag, so they always land in the same slot. Tests confirmed that `CLUSTER KEYSLOT` returns the same slot for the six data keys and the idempotency key, with the prefix in place.
- A script accesses only the keys passed in `KEYS`. If a key from another slot is mixed in, Redis rejects the call with `CROSSSLOT`.
- There are no cross-key operations between different selectors, so selectors can sit on different nodes without affecting each other.
- `MOVED` and `ASK` responses during slot migration are handled by the client library. The first call after a migration to a new node hits `NOSCRIPT`, which the fallback path in 6.1 handles.

### 11.2 Concurrency

- Redis runs scripts one at a time, so the "read, check, write" sequence inside a script cannot be interrupted by another request. Stock deduction, last-active protection and the candidate limit all depend on this, and no distributed lock is needed.
- Maintenance takes effect on every instance immediately: the next Pick reads the rebuilt `cum`. The component has no local cache.
- When two maintenance operations run concurrently, they execute in the order they reach Redis. Each one rebuilds from the result of the one before it.

### 11.3 Eventual Consistency of Lottery Stock

A script guarantees that stock never goes negative on a single Redis primary. One case is outside the script's reach. Redis replication is asynchronous, so if the primary fails before recent deductions reach a replica, the replica becomes primary with stock rolled back to an earlier value. Those units could then be issued again.

For prizes that must not be issued more than once, use two lines of defense:

1. Redis makes the fast decision and the deduction under high concurrency.
2. The database makes the final decision. The win record is written with a unique constraint, and the issued count of the prize is updated conditionally (the count goes up by one only while it is below the total). If that update fails, the draw is treated as a loss.

A business request's unique constraint, conditional deduction and issue log must be written in one database transaction. Confirm to the user only after the commit succeeds. External shipping must deduplicate by the issue serial number. An activity must use a selector ID that is never reused, so that a destroyed and recreated selector cannot match an old idempotency record. The idempotency window must cover the longest retry period.

When stronger consistency is required, you can append `WAIT 1 <milliseconds>` after the draw, sent on the same primary connection that ran the write, to wait for a replica to acknowledge. This adds latency and still is not a strict guarantee, so the database line of defense is required.

### 11.4 Failures and Timeouts

| Situation | Component behavior | Caller guidance |
| --- | --- | --- |
| `NOSCRIPT` | Automatically resends once with `EVAL` | Nothing to do |
| Network error or timeout | Returned as is, wrapped with `%w`; `errors.Is` can detect `context.DeadlineExceeded` | Pick can be retried directly, or served from the local fallback. Draw is retried with the same idempotency key |
| Redis unavailable | Returns an error | Supplier mode returns an error by default; enable the snapshot fallback in 10.4 explicitly if needed. Lottery mode asks the user to try again later |
| Inconsistent data (someone edited keys by hand) | Draw rebuilds inside the script and retries once; Pick returns `ErrInconsistent` directly | Nothing to do. For read-only accounts, see 9.6 |
| Selector does not exist | `ErrSelectorNotFound` | Check whether `Init` has been called, and whether the prefix and namespace match the writer's |
| Sold out, all disabled, or all excluded | `ErrNoAvailable` | Handle according to the business meaning: no prize, or every supplier failed |

Running scripts one at a time does not mean a runtime error rolls back earlier writes. Deploy with a dedicated key space and `noeviction`, and reserve memory. When an error type, a partial write or missing stock is found, stop issuing prizes, reconcile against the business ledger, and do not replay blindly.

The component does not retry automatically with backoff. The caller decides the retry policy, so that a Redis hiccup does not multiply traffic.

### 11.5 Performance

- Pick is one round trip with three Redis commands, and the script is read-only.
- Draw that wins an unlimited prize is one round trip with three read commands. Winning a limited prize adds one `HINCRBY`. Using an idempotency key adds one `GET` and one `SET`.
- Rebuilds happen only during maintenance and at the moment a prize sells out. Their cost is proportional to the number of candidates. Before going live, measure the tail latency of other requests on the same node under the candidate limit and your sell-out rate.
- `WithExclude` uses a linear scan. When there are many candidates and calls are frequent, reduce retries on the business side first.


---

## 12. Observability and Error Codes

### 12.1 Metrics and Logs

Metrics are emitted from the callback registered by `WithObserver`. Recommended metrics:

| Metric | Labels | Purpose |
| --- | --- | --- |
| `selector_script_total` | script, selector, result | Call volume and error rate. `result` is the error category; do not use error text as a label |
| `selector_script_duration_seconds` | script, selector | Duration distribution |
| `selector_pick_total` | selector, candidate | Actual traffic share, reported by the business side from `Result.CandidateID` |
| `selector_active_count`, `selector_total_weight` | selector | Reported periodically from `Describe`; used to alert when only one supplier remains |
| `selector_stock_remain` | selector, candidate | Stock level alerts |

Recommended alerts: the `active_count` of a selector equals `min_active`; `ErrLastActiveProtected` appears; `ErrInconsistent` appears (someone changed the data outside the component); or the high percentiles of script duration rise.

Log fields: selector_id, candidate_id, script, version, cost, error. Do not log seeds or `Ext`.

### 12.2 Error Codes

| Lua error code | Go error | Meaning |
| --- | --- | --- |
| None (local validation) | `ErrInvalidArgument` | Invalid ID, empty or duplicate candidates, oversized attributes, and similar |
| `ERR_SELECTOR_NOT_FOUND` | `ErrSelectorNotFound` | The selector has not been initialized |
| `ERR_SELECTOR_DISABLED` | `ErrSelectorDisabled` | The master switch is off |
| `ERR_MODE_MISMATCH` | `ErrModeMismatch` | Draw was called on a supplier selector, or the reverse; or `Init` was called with a different mode |
| `ERR_NO_AVAILABLE` | `ErrNoAvailable` | No candidate can be selected |
| `ERR_LAST_ACTIVE_PROTECTED` | `ErrLastActiveProtected` | Last-active protection rejected a disable or remove |
| `ERR_CANDIDATE_NOT_FOUND` | `ErrCandidateNotFound` | The candidate does not exist |
| `ERR_TOO_MANY_CANDIDATES` | `ErrTooManyCandidates` | The candidate limit was exceeded |
| `ERR_INVALID_WEIGHT` | `ErrInvalidWeight` | The weight is not an integer between 1 and the limit |
| `ERR_INVALID_STOCK` | `ErrInvalidStock` | The stock is invalid, or the deduction would take it below 0 |
| `ERR_STOCK_UNLIMITED` | `ErrStockUnlimited` | `AddStock` was called on an unlimited prize |
| `ERR_INCONSISTENT` | `ErrInconsistent` | The index is inconsistent. Pick returns it directly; Draw retries once for a rebuildable index error |
| Anything else | `ErrBadReply` | The reply does not match the contract, usually a version mismatch |

---

## 13. Testing and Verification

### 13.1 Tests in the Repository

| File | Type | Coverage |
| --- | --- | --- |
| `selector/unit_test.go`, `reply_test.go` | Unit tests, no Redis needed | Configuration validation, key layout and hash tags, invalid input never reaching Redis, `NOSCRIPT` fallback, network error propagation, error code mapping, `[]string` replies, malformed replies, lottery refusing a clock seed, idempotency key and TTL arguments, Observer, stock ceiling |
| `selector/supplier_integration_test.go` | Integration | 70/30 weight distribution, disable and enable, re-adding after removal, reweighting, last-active protection (disable and remove), concurrent disables where only one succeeds, retry with exclusion |
| `selector/lottery_integration_test.go` | Integration | 16 goroutines drawing concurrently never over-issue; sold-out prizes leave the pool; restocking; idempotent replay and TTL; re-registration and merge writes keep stock; everything sold out |
| `selector/fault_integration_test.go` | Integration | Missing selector, mode mismatch, master switch, destroy, candidate limit, `SCRIPT FLUSH`, key prefix and hash tag, missing stock field, broken index and `Rebuild` |
| `example/adapter_test.go` | Integration | The full path through the go-redis/v9 adapter |

The integration tests connect to Redis through a minimal RESP client in the test files, so the core module keeps zero dependencies. To run them:

```bash
go test ./... -race                                    # unit tests only
REDIS_ADDR=127.0.0.1:6379 go test ./... -race          # including integration tests
cd example && REDIS_ADDR=127.0.0.1:6379 go test ./...  # adapter end-to-end
```

Each test uses its own `KeyPrefix` and removes only the keys it wrote. `.github/workflows/ci.yml` runs the same tests in CI against a Redis service container.

Results for v2.2: Redis 8.6.3 standalone, Go 1.26.7. The 29 core tests passed under `-race`, and the adapter end-to-end test passed. Cluster mode was not re-verified in this round.

### 13.2 Historical Verification Record

The following is the v2.0 verification record. Environment: Redis 8.6.3 single node with Cluster mode enabled (all 16384 slots assigned, and the server enforces the cross-slot check), Go 1.26.7.

| Check | Result |
| --- | --- |
| Weights 70/30, 20,000 Pick calls, using a random seed, a TIME fallback seed, and consecutive integer seeds | Hit rate for A: 0.6994, 0.7025 and 0.7000 |
| 16 goroutines issuing 4000 Draw calls in total, with prizes limited to 20 and 200 | Exactly 20 and 200 issued; stock never went negative |
| Cumulative weights after a prize sells out | Only the unlimited prize remains; its score equals its own weight |
| Last-active protection: disabling or removing the last active supplier | Both rejected; the supplier kept being selected |
| Repeated Draw with the same idempotency key | Same result both times; stock deducted once; the idempotency key has a TTL |
| Merge write into an existing prize | Remaining stock unchanged |
| Inconsistency created by hand | Returns `ERR_INCONSISTENT` and completes a rebuild |
| Calls after `SCRIPT FLUSH` | Succeed after the `NOSCRIPT` fallback; verified with all four adapters |
| Slot of all keys with the prefix | Identical |
| Keys from another slot passed in | Rejected by the server with `CROSSSLOT` |
| The three ACL sets in 9.6 | The read-only account can Pick; it cannot maintain and cannot touch keys outside the prefix |
| Adapters | go-redis v9 standalone and cluster, v8 standalone, go-zero cluster mode, GoFrame standalone; all cases passed |

Not covered: slot migration and primary/replica failover in a multi-node cluster; the cluster modes of go-redis v8 and GoFrame; Redis 5 through 7 versions; latency under high-concurrency load tests. These should be added during implementation.

Recommended test layers for the implementation:

1. **Script tests**: run the scripts against a real Redis, covering every error code and boundary value. Do not replace this with an in-memory simulator, because Lua number precision and the behavior of `cjson` need a real environment.
2. **Core package tests**: use an in-memory fake `RedisClient` to verify input validation, reply parsing, the `NOSCRIPT` fallback and the retry logic.
3. **Adapter tests**: run the same cases against a real Redis for each adapter, once in standalone and once in cluster mode.
4. **Statistical tests**: run a chi-squared test on the selection distribution, with fixed seeds so the results are reproducible.
5. **Concurrency tests**: many goroutines compete for stock, and the test asserts that the number issued equals the stock. Many goroutines disable suppliers concurrently, and the test asserts that at least `min_active` remain.

---

## 14. Implementation Order

1. Core module: `types.go`, `core.go`, `ops.go` and the seven Lua files, implemented as described in this document, with script tests and core package tests.
2. The go-redis/v9 adapter, used as the baseline for passing the full set of integration cases.
3. The other three adapters, reusing the same integration cases.
4. The default Observer implementation (metrics and logs) and the local fallback decorator for supplier mode.
5. The admin console or admin API: authorization, operation audit, and a second confirmation for `Replace`.
6. A multi-node cluster rehearsal: scaling out and in, primary/replica failover and `SCRIPT FLUSH`, and verification of the ACL configuration in a staging environment.

