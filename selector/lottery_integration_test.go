package selector

import (
	"errors"
	"sync"
	"testing"
)

func initLottery(t *testing.T, s Selector, id string, cs ...Candidate) {
	t.Helper()
	if err := s.Init(bg, id, Spec{Mode: ModeLottery, Replace: true, Candidates: cs}); err != nil {
		t.Fatal(err)
	}
}

func stockOf(t *testing.T, s Selector, id, candidate string) int64 {
	t.Helper()
	snap, err := s.Describe(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Candidates {
		if c.ID == candidate {
			return c.Stock
		}
	}
	t.Fatalf("candidate %s not found", candidate)
	return 0
}

func TestConcurrentDrawNeverOversells(t *testing.T) {
	s, _, _ := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "big", Weight: 1, Stock: 20}, Candidate{ID: "small", Weight: 9, Stock: 100})

	var (
		mu     sync.Mutex
		won    = map[string]int{}
		failed []error
		wg     sync.WaitGroup
	)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				res, err := s.Draw(bg, "act")
				mu.Lock()
				switch {
				case err == nil:
					won[res.CandidateID]++
				case !errors.Is(err, ErrNoAvailable):
					failed = append(failed, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		t.Fatalf("unexpected errors: %v", failed[0])
	}
	// 800 requests compete for 120 items: all are issued and not one more.
	// 800 次请求抢 120 份库存：恰好全部发完，一份不多。
	if won["big"] != 20 || won["small"] != 100 {
		t.Fatalf("issued %v, want big=20 small=100", won)
	}
	if stockOf(t, s, "act", "big") != 0 || stockOf(t, s, "act", "small") != 0 {
		t.Fatal("stock not exhausted")
	}
}

func TestSoldOutPrizeLeavesPool(t *testing.T) {
	s, _, _ := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "rare", Weight: 99, Stock: 1}, Candidate{ID: "thanks", Weight: 1, Stock: -1})
	seen := map[string]int{}
	for i := 1; i <= 300; i++ {
		res, err := s.Draw(bg, "act", WithSeed(int64(i)))
		if err != nil {
			t.Fatal(err)
		}
		seen[res.CandidateID]++
		if res.CandidateID == "thanks" && res.RemainStock != -1 {
			t.Fatalf("unlimited prize reported stock %d", res.RemainStock)
		}
	}
	if seen["rare"] != 1 || seen["thanks"] != 299 {
		t.Fatalf("draws %v, want rare=1 thanks=299", seen)
	}
	// Restocking brings the prize back into the pool.
	// 补库存后奖品回到奖池。
	if err := s.AddStock(bg, "act", "rare", 2); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Describe(bg, "act")
	if snap.ActiveCount != 2 || snap.TotalWeight != 100 {
		t.Fatalf("restocked prize not active: %+v", snap)
	}
	if err := s.AddStock(bg, "act", "rare", -3); !errors.Is(err, ErrInvalidStock) {
		t.Fatalf("negative stock: %v", err)
	}
	if err := s.AddStock(bg, "act", "thanks", 1); !errors.Is(err, ErrStockUnlimited) {
		t.Fatalf("unlimited stock: %v", err)
	}
}

func TestDrawIdempotency(t *testing.T) {
	s, rdb, prefix := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 5})
	first, err := s.Draw(bg, "act", WithIdempotencyKey("order-1"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := s.Draw(bg, "act", WithIdempotencyKey("order-1"))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.CandidateID != first.CandidateID || again.RemainStock != first.RemainStock {
			t.Fatalf("replay %+v differs from first %+v", again, first)
		}
	}
	if first.Replayed || first.RemainStock != 4 || stockOf(t, s, "act", "gift") != 4 {
		t.Fatalf("stock deducted more than once: first=%+v", first)
	}
	ttl, err := rdb.do("TTL", prefix+"{rs:default:act}:idem:order-1")
	if err != nil || toInt(ttl) <= 0 || toInt(ttl) > 86400 {
		t.Fatalf("idempotency record ttl %v, err %v", ttl, err)
	}
	if other, _ := s.Draw(bg, "act", WithIdempotencyKey("order-2")); other == nil || other.Replayed || other.RemainStock != 3 {
		t.Fatalf("a different key must draw again: %+v", other)
	}
}

func TestMaintenanceKeepsRemainingStock(t *testing.T) {
	s, _, _ := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 5})
	for i := 0; i < 2; i++ {
		if _, err := s.Draw(bg, "act"); err != nil {
			t.Fatal(err)
		}
	}
	// Neither re-registering nor merging may give back stock that was already consumed.
	// 重复登记与合并写入都不能把已消耗的库存补回去。
	if err := s.Add(bg, "act", Candidate{ID: "gift", Weight: 3, Stock: 5}); err != nil {
		t.Fatal(err)
	}
	merge := Spec{Mode: ModeLottery, Candidates: []Candidate{{ID: "gift", Weight: 2, Stock: 5}, {ID: "new", Weight: 1, Stock: 7}}}
	if err := s.Init(bg, "act", merge); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, s, "act", "gift"); got != 3 {
		t.Fatalf("remaining stock %d, want 3", got)
	}
	if got := stockOf(t, s, "act", "new"); got != 7 {
		t.Fatalf("new prize stock %d, want 7", got)
	}
	// Replacing everything is the explicit reset.
	// 整体替换是显式的重置。
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 5})
	if got := stockOf(t, s, "act", "gift"); got != 5 {
		t.Fatalf("replace did not reset stock: %d", got)
	}
}

func TestAllSoldOut(t *testing.T) {
	s, _, _ := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 1})
	if _, err := s.Draw(bg, "act"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Draw(bg, "act"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("got %v, want ErrNoAvailable", err)
	}
	// Lottery mode has no last-active protection: the last prize can be taken down.
	// 抽奖模式没有末位保护：最后一个奖品可以下架。
	if err := s.AddStock(bg, "act", "gift", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(bg, "act", "gift"); err != nil {
		t.Fatalf("disable last prize: %v", err)
	}
}
