package selector

import (
	"context"
	"errors"
	"sync"
	"testing"
)

var bg = context.Background()

func initSuppliers(t *testing.T, s Selector, id string, cs ...Candidate) {
	t.Helper()
	if err := s.Init(bg, id, Spec{Mode: ModeSupplier, Replace: true, Candidates: cs}); err != nil {
		t.Fatal(err)
	}
}

func pickCounts(t *testing.T, s Selector, id string, n int, opts ...Option) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 1; i <= n; i++ {
		res, err := s.Pick(bg, id, append([]Option{WithSeed(int64(i))}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		counts[res.CandidateID]++
	}
	return counts
}

func TestSupplierWeightedDistribution(t *testing.T) {
	s, _, _ := newIntegration(t)
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 70}, Candidate{ID: "b", Weight: 30})
	const n = 5000
	counts := pickCounts(t, s, "pay", n)
	if ratio := float64(counts["a"]) / n; ratio < 0.67 || ratio > 0.73 {
		t.Fatalf("a ratio %.4f, want about 0.70 (%v)", ratio, counts)
	}
	res, err := s.Pick(bg, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"a": 70, "b": 30}[res.CandidateID]; res.Weight != want {
		t.Fatalf("weight %d for %s", res.Weight, res.CandidateID)
	}
}

func TestSupplierMaintenance(t *testing.T) {
	s, _, _ := newIntegration(t)
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1}, Candidate{ID: "b", Weight: 1})

	// A disabled supplier is no longer picked; enabling brings it back.
	// 停用后不再被选中，启用后恢复。
	if err := s.Disable(bg, "pay", "b"); err != nil {
		t.Fatal(err)
	}
	if c := pickCounts(t, s, "pay", 200); c["b"] != 0 {
		t.Fatalf("disabled supplier picked: %v", c)
	}
	if err := s.Enable(bg, "pay", "b"); err != nil {
		t.Fatal(err)
	}
	if c := pickCounts(t, s, "pay", 200); c["b"] == 0 {
		t.Fatalf("re-enabled supplier never picked: %v", c)
	}

	// After removal, Add registers it again; a weight change takes effect immediately.
	// 删除后用 Add 重新加入；改权重立即生效。
	if err := s.Remove(bg, "pay", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enable(bg, "pay", "b"); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("enable removed: %v", err)
	}
	if err := s.Add(bg, "pay", Candidate{ID: "b", Name: "B", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWeight(bg, "pay", "b", 99); err != nil {
		t.Fatal(err)
	}
	if c := pickCounts(t, s, "pay", 1000); c["b"] < 950 {
		t.Fatalf("weight change not applied: %v", c)
	}

	snap, err := s.Describe(bg, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != ModeSupplier || snap.ActiveCount != 2 || snap.TotalWeight != 100 || snap.MinActive != 1 {
		t.Fatalf("snapshot %+v", snap)
	}
	if b := snap.Candidates[1]; b.ID != "b" || b.Name != "B" || b.Weight != 99 || !b.Enabled {
		t.Fatalf("candidate %+v", b)
	}
}

func TestLastActiveSupplierIsProtected(t *testing.T) {
	s, _, _ := newIntegration(t)
	initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1}, Candidate{ID: "b", Weight: 1})
	if err := s.Disable(bg, "pay", "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(bg, "pay", "b"); !errors.Is(err, ErrLastActiveProtected) {
		t.Fatalf("disable last: %v", err)
	}
	if err := s.Remove(bg, "pay", "b"); !errors.Is(err, ErrLastActiveProtected) {
		t.Fatalf("remove last: %v", err)
	}
	// A rejected call changes nothing: b is still served.
	// 被拒绝后状态不变：b 仍然可选。
	if c := pickCounts(t, s, "pay", 50); c["b"] != 50 {
		t.Fatalf("last supplier not served: %v", c)
	}
	// The disabled a is out of the pool, so removing it is not guarded.
	// 已停用的 a 不参与选择，删除它不受保护限制。
	if err := s.Remove(bg, "pay", "a"); err != nil {
		t.Fatalf("remove disabled: %v", err)
	}
	// Init cannot empty the suppliers or set the lower bound to 0 either.
	// 整体替换与合并写入也不能把供应商清空或把下限设为 0。
	zero := int64(0)
	err := s.Init(bg, "pay", Spec{Mode: ModeSupplier, MinActive: &zero, Candidates: []Candidate{{ID: "c", Weight: 1}}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("min active 0: %v", err)
	}
}

func TestConcurrentDisableKeepsOneSupplier(t *testing.T) {
	s, _, _ := newIntegration(t)
	for round := 0; round < 20; round++ {
		initSuppliers(t, s, "pay", Candidate{ID: "a", Weight: 1}, Candidate{ID: "b", Weight: 1})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, id := range []string{"a", "b"} {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				errs[i] = s.Disable(bg, "pay", id)
			}(i, id)
		}
		wg.Wait()
		protected := 0
		for _, err := range errs {
			if errors.Is(err, ErrLastActiveProtected) {
				protected++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if protected != 1 {
			t.Fatalf("round %d: %d calls rejected, want exactly 1", round, protected)
		}
		if _, err := s.Pick(bg, "pay"); err != nil {
			t.Fatalf("round %d: no supplier left: %v", round, err)
		}
	}
}

func TestPickWithExclude(t *testing.T) {
	s, _, _ := newIntegration(t)
	initSuppliers(t, s, "pay",
		Candidate{ID: "a", Weight: 80}, Candidate{ID: "b", Weight: 15}, Candidate{ID: "c", Weight: 5})
	c := pickCounts(t, s, "pay", 2000, WithExclude("a"))
	if c["a"] != 0 {
		t.Fatalf("excluded supplier picked: %v", c)
	}
	if ratio := float64(c["b"]) / 2000; ratio < 0.70 || ratio > 0.80 {
		t.Fatalf("b ratio %.3f after exclusion, want about 0.75", ratio)
	}
	if _, err := s.Pick(bg, "pay", WithExclude("a", "b", "c")); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("all excluded: %v", err)
	}
	// Exclusion affects this call only.
	// 排除只影响本次调用。
	if c := pickCounts(t, s, "pay", 200); c["a"] == 0 {
		t.Fatal("exclusion leaked into later picks")
	}
}
