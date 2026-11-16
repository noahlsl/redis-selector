package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/noahlsl/redis-selector/selector"
)

// MetricsObserver records one counter/duration sample per Lua script call.
// MetricsObserver 在每次 Lua 脚本执行后记录一次计数与耗时。
type MetricsObserver struct {
	mu     sync.Mutex
	totals map[string]int
	durSum map[string]time.Duration
}

func NewMetricsObserver() *MetricsObserver {
	return &MetricsObserver{
		totals: make(map[string]int),
		durSum: make(map[string]time.Duration),
	}
}

func (m *MetricsObserver) Observe(ctx context.Context, e selector.Event) {
	result := errorCategory(e.Err)
	key := e.Script + "|" + e.SelectorID + "|" + result

	m.mu.Lock()
	m.totals[key]++
	m.durSum[key] += e.Cost
	m.mu.Unlock()

	// Attach script-level attributes to the current business span when present.
	// 如果当前业务 span 存在，把脚本级属性挂上去。
	if span := SpanFromContext(ctx); span != nil {
		span.SetAttributes(
			"selector.script", e.Script,
			"selector.id", e.SelectorID,
			"selector.script_result", result,
			"selector.script_cost_ms", e.Cost.Milliseconds(),
		)
		if e.Err != nil {
			span.RecordError(e.Err)
		}
	}

	if e.Err != nil {
		log.Printf("script=%s selector=%s cost=%s result=%s err=%v", e.Script, e.SelectorID, e.Cost, result, e.Err)
		return
	}
	log.Printf("script=%s selector=%s cost=%s result=%s", e.Script, e.SelectorID, e.Cost, result)
}

func (m *MetricsObserver) Snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.totals))
	for k, v := range m.totals {
		out[k] = v
	}
	return out
}

func errorCategory(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, selector.ErrInvalidArgument):
		return "invalid_argument"
	case errors.Is(err, selector.ErrSelectorNotFound):
		return "selector_not_found"
	case errors.Is(err, selector.ErrSelectorDisabled):
		return "selector_disabled"
	case errors.Is(err, selector.ErrModeMismatch):
		return "mode_mismatch"
	case errors.Is(err, selector.ErrNoAvailable):
		return "no_available"
	case errors.Is(err, selector.ErrLastActiveProtected):
		return "last_active_protected"
	case errors.Is(err, selector.ErrCandidateNotFound):
		return "candidate_not_found"
	case errors.Is(err, selector.ErrTooManyCandidates):
		return "too_many_candidates"
	case errors.Is(err, selector.ErrInvalidWeight):
		return "invalid_weight"
	case errors.Is(err, selector.ErrInvalidStock):
		return "invalid_stock"
	case errors.Is(err, selector.ErrStockUnlimited):
		return "stock_unlimited"
	case errors.Is(err, selector.ErrInconsistent):
		return "inconsistent"
	case errors.Is(err, selector.ErrBadReply):
		return "bad_reply"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}
