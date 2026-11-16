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
// loadScripts 把 lib.lua 拼到每个脚本前面，并在本地算出 SHA1。
// 本地算 SHA 就不需要 SCRIPT LOAD：Cluster 下 SCRIPT LOAD 只落到一个节点，靠它预热并不可靠。
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
	seedMax      = 2147483646 // seeds fall in [1, 2^31-2] / 种子取值 [1, 2^31-2]
	maxItemBytes = 4096
	maxStock     = 1 << 53 // Lua numbers are doubles; stock above 2^53 cannot be compared exactly / Lua 数值是 double，超过 2^53 的库存无法精确比较
)

// checkID rejects braces, colons, whitespace and control characters: braces change the hash tag, and colons let different (namespace, id) pairs build the same key.
// checkID 拒绝花括号、冒号、空白与控制字符：花括号会改变 Hash Tag，冒号会让不同 (namespace, id) 拼出同一个 Key。
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
// cryptoSeed 用密码学安全随机源生成种子，外部无法预测抽奖结果。
// 失败时返回 0，脚本会退化为用 TIME 做种子。
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
// keys 生成 [KeyPrefix]{rs:<namespace>:<selector_id>}:<suffix>。
// 前缀在 Hash Tag 左侧，不参与 Slot 计算；同一选择器的所有 Key 落在同一个 Slot。
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
// run 先 EVALSHA，遇到 NOSCRIPT 再用 EVAL 带源码执行一次（同时完成该节点的脚本缓存）。
// 成功时返回去掉状态位之后的回复。
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
// 不同客户端对数组回复的表示不同：go-redis、go-zero 是 []any（整数为 int64），GoFrame 是 []string。
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
