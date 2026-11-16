package selector

import (
	"errors"
	"strings"
	"testing"
)

func TestSelectorStateErrors(t *testing.T) {
	s, _, _ := newIntegration(t)
	if _, err := s.Pick(bg, "missing"); !errors.Is(err, ErrSelectorNotFound) {
		t.Fatalf("pick missing: %v", err)
	}
	if _, err := s.Describe(bg, "missing"); !errors.Is(err, ErrSelectorNotFound) {
		t.Fatalf("describe missing: %v", err)
	}
	if err := s.Add(bg, "missing", Candidate{ID: "a", Weight: 1}); !errors.Is(err, ErrSelectorNotFound) {
		t.Fatalf("add to missing: %v", err)
	}

	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1})
	if _, err := s.Draw(bg, "pay"); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("draw on supplier selector: %v", err)
	}
	if err := s.AddStock(bg, "pay", "a", 1); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("add stock on supplier selector: %v", err)
	}
	err := s.Init(bg, "pay", Spec{Mode: ModeLottery, Replace: true, Candidates: []Candidate{{ID: "a", Weight: 1}}})
	if !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("re-init with another mode: %v", err)
	}

	// Master switch: off rejects selection and keeps the data; on restores it.
	// 总开关：关闭后拒绝选择，数据保留，打开后恢复。
	if err := s.SetSelectorEnabled(bg, "pay", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick(bg, "pay"); !errors.Is(err, ErrSelectorDisabled) {
		t.Fatalf("pick while disabled: %v", err)
	}
	if err := s.SetSelectorEnabled(bg, "pay", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick(bg, "pay"); err != nil {
		t.Fatal(err)
	}

	if err := s.Destroy(bg, "pay"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick(bg, "pay"); !errors.Is(err, ErrSelectorNotFound) {
		t.Fatalf("pick after destroy: %v", err)
	}
}

func TestCandidateLimitIsEnforcedInRedis(t *testing.T) {
	s, _, _ := newIntegration(t, WithMaxCandidates(2))
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1}, Candidate{ID: "b", Weight: 1})
	if err := s.Add(bg, "pay", Candidate{ID: "c", Weight: 1}); !errors.Is(err, ErrTooManyCandidates) {
		t.Fatalf("add beyond limit: %v", err)
	}
	merge := Spec{Mode: ModeSupplier, Candidates: []Candidate{{ID: "c", Weight: 1}}}
	if err := s.Init(bg, "pay", merge); !errors.Is(err, ErrTooManyCandidates) {
		t.Fatalf("merge beyond limit: %v", err)
	}
	// A rejected merge must leave no partial write.
	// 被拒绝的合并不能留下部分写入。
	snap, err := s.Describe(bg, "pay")
	if err != nil || len(snap.Candidates) != 2 {
		t.Fatalf("snapshot %+v, err %v", snap, err)
	}
	// Overwriting an existing candidate takes no new slot.
	// 覆盖已有候选不占用新名额。
	if err := s.Add(bg, "pay", Candidate{ID: "a", Weight: 5}); err != nil {
		t.Fatal(err)
	}
}

func TestScriptCacheFlushIsTransparent(t *testing.T) {
	s, rdb, _ := newIntegration(t)
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1})
	if _, err := rdb.do("SCRIPT", "FLUSH"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // first call falls back to EVAL, second is back on EVALSHA / 第一次走 EVAL 回退，第二次回到 EVALSHA
		if _, err := s.Pick(bg, "pay"); err != nil {
			t.Fatalf("pick %d after flush: %v", i, err)
		}
	}
}

func TestKeysStayUnderPrefixAndHashTag(t *testing.T) {
	s, rdb, prefix := newIntegration(t, WithNamespace("ns1"))
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 3})
	if _, err := s.Draw(bg, "act", WithIdempotencyKey("o1")); err != nil {
		t.Fatal(err)
	}
	keys, err := rdb.do("KEYS", prefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(toSlice(keys)); n != 7 { // meta cum weight stock enabled items idem
		t.Fatalf("%d keys written, want 7: %v", n, keys)
	}
	for _, k := range toSlice(keys) {
		if !strings.HasPrefix(toString(k), prefix+"{rs:ns1:act}:") {
			t.Fatalf("key %v escapes the prefix or hash tag", k)
		}
	}
}

func TestDrawRejectsMissingStock(t *testing.T) {
	s, rdb, prefix := newIntegration(t)
	initLottery(t, s, "act", Candidate{ID: "gift", Weight: 1, Stock: 3})
	// The stock field was deleted externally: it must not be treated as unlimited.
	// 库存字段被外部删除：不能当作不限量继续发奖。
	if _, err := rdb.do("HDEL", prefix+"{rs:default:act}:stock", "gift"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Draw(bg, "act"); !errors.Is(err, ErrInvalidStock) {
		t.Fatalf("got %v, want ErrInvalidStock", err)
	}
	// After a rebuild the prize leaves the pool until the admin side registers it again.
	// 重建后该奖品退出奖池，直到管理端重新登记。
	if err := s.Rebuild(bg, "act"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Draw(bg, "act"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("after rebuild: %v", err)
	}
}

func TestStaleIndexHandling(t *testing.T) {
	s, rdb, prefix := newIntegration(t)

	// Lottery: when a zero-stock prize lingers in the index, the script rebuilds and retries; the caller notices nothing.
	// 抽奖：索引里残留零库存奖品时，脚本重建后重试，调用方无感知。
	initLottery(t, s, "act", Candidate{ID: "gone", Weight: 1, Stock: 1}, Candidate{ID: "thanks", Weight: 1, Stock: -1})
	if _, err := rdb.do("HSET", prefix+"{rs:default:act}:stock", "gone", 0); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		res, err := s.Draw(bg, "act", WithSeed(int64(i)))
		if err != nil || res.CandidateID != "thanks" {
			t.Fatalf("draw %d: %+v, %v", i, res, err)
		}
	}

	// Supplier: Pick is read-only and reports a broken index; Rebuild repairs it.
	// 供应商：Pick 只读，发现索引损坏时报错；Rebuild 修复。
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1})
	if _, err := rdb.do("DEL", prefix+"{rs:default:pay}:cum"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick(bg, "pay"); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("pick with broken index: %v", err)
	}
	if err := s.Rebuild(bg, "pay"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Pick(bg, "pay"); err != nil || res.CandidateID != "a" {
		t.Fatalf("pick after rebuild: %+v, %v", res, err)
	}
}
