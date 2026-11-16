package selector

import (
	"context"
	"errors"
	"testing"
)

func TestLuaErrorCodesMapToSentinels(t *testing.T) {
	for code, want := range luaErrors {
		f := &fakeRedis{reply: []any{int64(0), code}}
		s := mustNew(t, f)
		if err := s.Disable(context.Background(), "pay", "a"); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", code, err, want)
		}
	}
	f := &fakeRedis{reply: []any{int64(0), "ERR_FROM_NEWER_SCRIPT"}}
	s := mustNew(t, f)
	if err := s.Disable(context.Background(), "pay", "a"); !errors.Is(err, ErrBadReply) {
		t.Errorf("unknown code: got %v, want ErrBadReply", err)
	}
}

// GoFrame's gredis turns array replies into []string, so integers arrive as strings too.
// GoFrame 的 gredis 把数组回复转成 []string，整数也以字符串形式出现。
func TestStringSliceReply(t *testing.T) {
	f := &fakeRedis{reply: []string{"1", "gift", "4", "9", "1"}}
	s := mustNew(t, f)
	res, err := s.Draw(context.Background(), "act")
	if err != nil {
		t.Fatal(err)
	}
	if res.CandidateID != "gift" || res.RemainStock != 4 || res.Version != 9 || !res.Replayed {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestMalformedReplies(t *testing.T) {
	for name, reply := range map[string]any{
		"nil":         nil,
		"scalar":      "OK",
		"empty":       []any{},
		"status only": []any{int64(1)},
		"short pick":  []any{int64(1), "a"},
	} {
		f := &fakeRedis{reply: reply}
		s := mustNew(t, f)
		if _, err := s.Pick(context.Background(), "pay"); !errors.Is(err, ErrBadReply) {
			t.Errorf("%s: got %v, want ErrBadReply", name, err)
		}
	}
}

// A draw must not fall back to a predictable clock seed: without a random source it fails before reaching Redis.
// 抽奖不允许退化到可预测的时钟种子：随机源不可用时直接失败，不访问 Redis。
func TestDrawRefusesWithoutSeed(t *testing.T) {
	f := &fakeRedis{reply: []any{int64(1), "a", int64(-1), int64(1), int64(0)}}
	s := mustNew(t, f, WithSeedFunc(func() int64 { return 0 }))
	if _, err := s.Draw(context.Background(), "act"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if f.shaCalls != 0 {
		t.Fatal("draw reached redis without a seed")
	}
	// Supplier picks may let the script fall back to TIME.
	// 供应商选择允许脚本用 TIME 兜底。
	f.reply = []any{int64(1), "a", int64(1), int64(1)}
	if _, err := s.Pick(context.Background(), "pay"); err != nil {
		t.Fatal(err)
	}
}

func TestDrawPassesIdempotencyKeyAndTTL(t *testing.T) {
	f := &fakeRedis{reply: []any{int64(1), "a", int64(-1), int64(1), int64(0)}}
	s := mustNew(t, f, WithKeyPrefix("p"))
	if _, err := s.Draw(context.Background(), "act", WithIdempotencyKey("order-1")); err != nil {
		t.Fatal(err)
	}
	if len(f.lastKeys) != 6 || f.lastKeys[5] != "p:{rs:default:act}:idem:order-1" {
		t.Fatalf("keys %v", f.lastKeys)
	}
	if toInt(f.lastArgs[1]) != 86400 {
		t.Fatalf("ttl arg %v, want 86400", f.lastArgs[1])
	}
	if seed := toInt(f.lastArgs[0]); seed < 1 || seed > seedMax {
		t.Fatalf("seed %d out of range", seed)
	}
}

func TestObserverSeesEveryScriptCall(t *testing.T) {
	var events []Event
	f := &fakeRedis{reply: []any{int64(0), "ERR_LAST_ACTIVE_PROTECTED"}}
	s := mustNew(t, f, WithObserver(func(_ context.Context, e Event) { events = append(events, e) }))
	_ = s.Disable(context.Background(), "pay", "a")
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	if e.Script != "selector_mutate" || e.SelectorID != "pay" || !errors.Is(e.Err, ErrLastActiveProtected) {
		t.Fatalf("unexpected event %+v", e)
	}
}

func TestStockBounds(t *testing.T) {
	f := &fakeRedis{reply: []any{int64(1), int64(1), int64(1), int64(1)}}
	s := mustNew(t, f)
	ctx := context.Background()
	if err := s.AddStock(ctx, "act", "a", maxStock+1); !errors.Is(err, ErrInvalidStock) {
		t.Errorf("AddStock: got %v", err)
	}
	err := s.Init(ctx, "act", Spec{Mode: ModeLottery, Candidates: []Candidate{{ID: "a", Weight: 1, Stock: maxStock + 1}}})
	if !errors.Is(err, ErrInvalidStock) {
		t.Errorf("Init: got %v", err)
	}
	if f.shaCalls != 0 {
		t.Fatal("out-of-range stock reached redis")
	}
}
