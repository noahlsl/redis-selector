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
// Draw 抽奖。库存扣减在脚本内完成；带幂等键时，超时后用同一个键重试不会重复扣减。
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
			break // the script already rebuilt the index when it found the inconsistency; one retry is enough / 脚本发现数据不一致时已自行重建，这里只需重试一次
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
// Pick 按权重随机选一个供应商。全程只读，发现不一致时返回错误，由管理端排查。
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
// Init 创建选择器或批量写入候选。写入与重建在一个脚本内完成，其他实例不会读到半成品。
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
	// 合并写入时已有候选也计入可用数量，最终是否满足下限由脚本判断。
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
// Add 是幂等的 upsert：已存在则覆盖权重与属性并重新启用。仅首次登记奖品时设置初始库存；
// supplier 模式忽略 Stock。
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
// Disable 把候选临时移出选择池，保留其配置，之后可用 Enable 加回。
// 如果它是最后一个可用候选，返回 ErrLastActiveProtected，状态不变。
func (s *selector) Disable(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "disable", candidateID, 0, 0, "")
}

func (s *selector) Enable(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "enable", candidateID, 0, 0, "")
}

// Remove deletes a candidate and its configuration; only Add can bring it back. Last-active protection applies as in Disable.
// Remove 彻底删除候选及其配置，之后只能用 Add 重新加入。末位保护规则同 Disable。
func (s *selector) Remove(ctx context.Context, selectorID, candidateID string) error {
	return s.mutate(ctx, selectorID, "remove", candidateID, 0, 0, "")
}

// AddStock adjusts the stock of a limited prize. delta may be negative; the result cannot go below 0.
// AddStock 增减限量奖品的库存。delta 可以为负，结果不能小于 0。
func (s *selector) AddStock(ctx context.Context, selectorID, candidateID string, delta int64) error {
	if delta > maxStock || delta < -maxStock {
		return fmt.Errorf("%w: delta %d", ErrInvalidStock, delta)
	}
	return s.mutate(ctx, selectorID, "incr_stock", candidateID, 0, delta, "")
}

// Rebuild regenerates the cumulative weight table from weight, enabled and stock without changing any configuration.
// Use it to repair the index after ErrInconsistent, once the source data has been checked.
// Rebuild 按 weight、enabled、stock 重新生成累积权重表，不改变任何配置。
// 用于 ErrInconsistent 出现后、核对过源数据再修复索引。
func (s *selector) Rebuild(ctx context.Context, selectorID string) error {
	return s.mutate(ctx, selectorID, "rebuild", "", 0, 0, "")
}

// SetSelectorEnabled is the master switch of a selector; when off, Pick and Draw return ErrSelectorDisabled.
// SetSelectorEnabled 是整个选择器的总开关，关闭后 Pick 与 Draw 返回 ErrSelectorDisabled。
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
		c := CandidateState{Enabled: raw.Enabled[id] == "1"} // same condition the rebuild function uses / 与重建函数的入选条件一致
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
