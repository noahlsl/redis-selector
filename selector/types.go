package selector

import (
	"context"
	"errors"
	"time"
)

// RedisClient is everything this package needs from Redis. All reads and writes go through Lua, so two methods are enough.
// Contract: script replies are flat arrays of scalars; adapters return them untouched ([]any and []string are both accepted).
// RedisClient 是组件对 Redis 的全部依赖。所有读写都走 Lua，所以只需要这两个方法。
// 约定：脚本回复是只含标量的一维数组，适配器原样返回即可（[]any 或 []string 都接受）。
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
// Candidate 是一个奖品或一个供应商。
type Candidate struct {
	ID     string
	Name   string
	Weight int64             // 1..MaxWeight (see WithMaxWeight)
	Stock  int64             // lottery only: -1 unlimited, >=0 stock; ignored in supplier mode / 仅 lottery：-1 不限量，>=0 为库存；supplier 模式忽略
	Ext    map[string]string // display-only extension fields; never store secrets here / 展示用扩展字段，禁止存放密钥等敏感信息
}

// Spec describes one initialization or batch write.
// Spec 描述一次初始化或批量写入。
type Spec struct {
	Mode       Mode
	MinActive  *int64 // lower bound of active candidates; nil uses the default: supplier=1, lottery=0 / 可用候选的下限；nil 取默认值：supplier=1，lottery=0
	Replace    bool   // true replaces everything (stock is reset), false merges (disabled state and remaining stock are kept) / true 整体替换（会重置库存），false 合并（保留停用状态与剩余库存）
	Candidates []Candidate
}

// Result is the outcome of Pick or Draw.
// Result 是 Pick 或 Draw 的结果。
type Result struct {
	SelectorID  string
	CandidateID string
	Weight      int64 // returned by Pick / Pick 返回
	RemainStock int64 // returned by Draw; -1 means unlimited / Draw 返回；-1 表示不限量
	Version     int64 // weight table version, incremented on every rebuild / 权重表版本，每次重建加一
	Replayed    bool  // Draw hit an idempotency key; no stock was deducted this time / Draw 命中幂等键，本次没有再扣库存
}

type CandidateState struct {
	Candidate
	Enabled bool
}

// Snapshot is the full state of a selector, for admin consoles and local fallback.
// Snapshot 是选择器的完整状态，供管理后台展示和本地兜底使用。
type Snapshot struct {
	Mode        Mode
	Enabled     bool
	Version     int64
	TotalWeight int64
	ActiveCount int64
	MinActive   int64
	Candidates  []CandidateState // sorted by ID ascending / 按 ID 升序
}

// Picker and Drawer are the hot-path interfaces; Admin is the maintenance interface.
// Business code depends only on the first two; inject Admin separately to isolate privileges.
// Picker 与 Drawer 是业务热路径接口，Admin 是维护接口。
// 业务代码只依赖前两者，维护入口单独注入，便于做权限隔离。
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
// config 在构造时确定，之后不可变。KeyPrefix 与 Namespace 决定 Key 的位置，
// 因此只允许实例级配置，不提供单次调用覆盖。
type config struct {
	KeyPrefix     string        // optional prefix such as "pay:prod:"; empty means none / 可选前缀，如 "pay:prod:"；空表示不加
	Namespace     string        // defaults to "default" / 默认 "default"
	MaxCandidates int           // defaults to 1000 / 默认 1000
	MaxWeight     int64         // defaults to 1_000_000 / 默认 1_000_000
	IdemTTL       time.Duration // how long idempotency records are kept, defaults to 24h / 幂等键保留时长，默认 24h
	Seed          func() int64  // seed source, defaults to crypto/rand; must return a value in [1, 2^31-2] / 随机种子来源，默认 crypto/rand；返回值需在 [1, 2^31-2]
	Observer      func(ctx context.Context, e Event)
}

// ConfigOption configures a Selector at construction time.
// Later options override earlier ones for the same field.
// ConfigOption 用于构造时配置 Selector；同一字段后写覆盖先写。
type ConfigOption func(*config)

// WithKeyPrefix sets an optional key prefix such as "pay:prod".
// WithKeyPrefix 设置可选 Key 前缀，例如 "pay:prod"。
func WithKeyPrefix(prefix string) ConfigOption {
	return func(c *config) { c.KeyPrefix = prefix }
}

// WithNamespace sets the namespace. Empty values still fall back to "default" in New.
// WithNamespace 设置命名空间；空值仍会在 New 中回退为 "default"。
func WithNamespace(ns string) ConfigOption {
	return func(c *config) { c.Namespace = ns }
}

// WithMaxCandidates sets the candidate ceiling. Zero or negative values keep the default.
// WithMaxCandidates 设置候选上限；零或负值保留默认值。
func WithMaxCandidates(n int) ConfigOption {
	return func(c *config) { c.MaxCandidates = n }
}

// WithMaxWeight sets the weight ceiling. Zero or negative values keep the default.
// WithMaxWeight 设置权重上限；零或负值保留默认值。
func WithMaxWeight(w int64) ConfigOption {
	return func(c *config) { c.MaxWeight = w }
}

// WithIdemTTL sets how long idempotency records are kept.
// WithIdemTTL 设置幂等记录保留时长。
func WithIdemTTL(ttl time.Duration) ConfigOption {
	return func(c *config) { c.IdemTTL = ttl }
}

// WithSeedFunc sets the instance seed source used when a call does not pass WithSeed.
// Do not confuse with WithSeed, which fixes one call for tests/reproduction only.
// WithSeedFunc 设置实例级种子来源；调用未传 WithSeed 时使用。
// 不要和 WithSeed 混淆：WithSeed 只固定单次调用，用于测试与复现。
func WithSeedFunc(fn func() int64) ConfigOption {
	return func(c *config) { c.Seed = fn }
}

// WithObserver registers a callback invoked after every script execution.
// WithObserver 注册脚本执行后的回调。
func WithObserver(fn func(ctx context.Context, e Event)) ConfigOption {
	return func(c *config) { c.Observer = fn }
}

// Event is reported once after every script execution, for metrics, logs, tracing and auditing.
// Event 在每次脚本执行后回调一次，用于指标、日志、链路追踪与审计。
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
// WithSeed 固定随机种子，仅用于测试与问题复现。不要把外部请求里的值传进来。
func WithSeed(seed int64) Option { return func(o *options) { o.seed = seed } }

// WithIdempotencyKey makes repeated Draw calls with the same key return the first result (use an order or request number).
// WithIdempotencyKey 让同一个 key 的重复 Draw 返回首次结果（如订单号、请求号）。
func WithIdempotencyKey(key string) Option { return func(o *options) { o.idemKey = key } }

// WithExclude makes this Pick skip the given suppliers, for retrying with another supplier after a failed call.
// WithExclude 让本次 Pick 跳过指定供应商，用于调用失败后换一家重试。
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
