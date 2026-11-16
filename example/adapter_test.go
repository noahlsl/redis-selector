package example_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/noahlsl/redis-selector/adapter/goredisv9"
	"github.com/noahlsl/redis-selector/selector"
	"github.com/redis/go-redis/v9"
)

// Exercise the real path through the go-redis/v9 adapter: reply types, NOSCRIPT fallback and error propagation.
// Requires REDIS_ADDR; skipped when it is unset.
// 通过 go-redis/v9 适配器走一遍真实链路：回复类型、NOSCRIPT 回退、错误透传。
// 需要 REDIS_ADDR，未设置时跳过。
func TestGoRedisV9Adapter(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run adapter tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() }) // registered first, runs last: the cleanup below still has a live connection / 先注册、最后执行：保证下面的清理仍有可用连接
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prefix := fmt.Sprintf("rstest:adapter-%d", time.Now().UnixNano())
	sel, err := selector.New(goredisv9.New(rdb), selector.WithKeyPrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sel.Destroy(context.Background(), "pay")
		_ = sel.Destroy(context.Background(), "act")
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})

	err = sel.Init(ctx, "pay", selector.Spec{Mode: selector.ModeSupplier, Replace: true, Candidates: []selector.Candidate{
		{ID: "a", Name: "A", Weight: 70, Ext: map[string]string{"region": "eu"}},
		{ID: "b", Weight: 30},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	res, err := sel.Pick(ctx, "pay", selector.WithExclude("b"))
	if err != nil || res.CandidateID != "a" || res.Weight != 70 {
		t.Fatalf("pick: %+v, %v", res, err)
	}
	if err := sel.Disable(ctx, "pay", "b"); err != nil {
		t.Fatal(err)
	}
	if err := sel.Disable(ctx, "pay", "a"); !errors.Is(err, selector.ErrLastActiveProtected) {
		t.Fatalf("last active: %v", err)
	}
	snap, err := sel.Describe(ctx, "pay")
	if err != nil || len(snap.Candidates) != 2 || snap.Candidates[0].Ext["region"] != "eu" || snap.Candidates[1].Enabled {
		t.Fatalf("describe: %+v, %v", snap, err)
	}

	err = sel.Init(ctx, "act", selector.Spec{Mode: selector.ModeLottery, Replace: true, Candidates: []selector.Candidate{
		{ID: "gift", Weight: 1, Stock: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := sel.Draw(ctx, "act", selector.WithIdempotencyKey("o1"))
	if err != nil || first.RemainStock != 0 {
		t.Fatalf("draw: %+v, %v", first, err)
	}
	if again, err := sel.Draw(ctx, "act", selector.WithIdempotencyKey("o1")); err != nil || !again.Replayed {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if _, err := sel.Draw(ctx, "act"); !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("sold out: %v", err)
	}
}
