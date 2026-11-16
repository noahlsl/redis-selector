package selector

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRedis records calls and returns a canned reply, for logic that needs no real Redis.
// fakeRedis 记录调用并返回预设回复，用于验证不需要真实 Redis 的逻辑。
type fakeRedis struct {
	shaErr, evalErr error
	reply           any
	shaCalls        int
	evalCalls       int
	lastSha         string
	lastSrc         string
	lastKeys        []string
	lastArgs        []any
}

func (f *fakeRedis) EvalSha(_ context.Context, sha string, keys []string, args ...any) (any, error) {
	f.shaCalls++
	f.lastSha, f.lastKeys, f.lastArgs = sha, keys, args
	if f.shaErr != nil {
		return nil, f.shaErr
	}
	return f.reply, nil
}

func (f *fakeRedis) Eval(_ context.Context, src string, keys []string, args ...any) (any, error) {
	f.evalCalls++
	f.lastSrc, f.lastKeys, f.lastArgs = src, keys, args
	if f.evalErr != nil {
		return nil, f.evalErr
	}
	return f.reply, nil
}

func mustNew(t *testing.T, cli RedisClient, opts ...ConfigOption) Selector {
	t.Helper()
	s, err := New(cli, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewValidatesConfig(t *testing.T) {
	bad := map[string][]ConfigOption{
		"brace in prefix":   {WithKeyPrefix("a{b}")},
		"space in prefix":   {WithKeyPrefix("a b")},
		"colon in ns":       {WithNamespace("a:b")},
		"too many":          {WithMaxCandidates(1001)},
		"weight too large":  {WithMaxWeight(1_000_001)},
		"sub-second ttl":    {WithIdemTTL(500 * time.Millisecond)},
		"fractional ttl":    {WithIdemTTL(1500 * time.Millisecond)},
		"negative ttl":      {WithIdemTTL(-time.Second)},
		"prefix over limit": {WithKeyPrefix(strings.Repeat("a", 65))},
		"nil option":        {nil},
	}
	for name, opts := range bad {
		if _, err := New(&fakeRedis{}, opts...); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: got %v, want ErrInvalidArgument", name, err)
		}
	}
	if _, err := New(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("nil client: got %v", err)
	}
}

func TestKeyLayout(t *testing.T) {
	cases := []struct{ prefix, ns, want string }{
		{"", "", "{rs:default:pay}:meta"},
		{"app:prod", "supplier", "app:prod:{rs:supplier:pay}:meta"},
		{"app:prod:", "supplier", "app:prod:{rs:supplier:pay}:meta"},
	}
	for _, c := range cases {
		f := &fakeRedis{reply: []any{int64(1), "a", int64(70), int64(3)}}
		s := mustNew(t, f, WithKeyPrefix(c.prefix), WithNamespace(c.ns))
		if _, err := s.Pick(context.Background(), "pay"); err != nil {
			t.Fatal(err)
		}
		if f.lastKeys[0] != c.want {
			t.Errorf("prefix %q: meta key %q, want %q", c.prefix, f.lastKeys[0], c.want)
		}
		// All keys of one selector must share one hash tag, otherwise Cluster rejects the script.
		// 同一选择器的 Key 必须共享同一个 Hash Tag，否则 Cluster 会拒绝执行。
		tag := c.want[strings.Index(c.want, "{") : strings.Index(c.want, "}")+1]
		for _, k := range f.lastKeys {
			if !strings.Contains(k, tag) {
				t.Errorf("key %q misses hash tag %q", k, tag)
			}
		}
	}
}

func TestInvalidIDsNeverReachRedis(t *testing.T) {
	f := &fakeRedis{reply: []any{int64(1), "a", int64(1), int64(1)}}
	s := mustNew(t, f)
	ctx := context.Background()
	calls := []error{
		func() error { _, err := s.Pick(ctx, "a}:{b"); return err }(),
		func() error { _, err := s.Pick(ctx, ""); return err }(),
		func() error { _, err := s.Pick(ctx, "ok", WithExclude("x y")); return err }(),
		func() error { _, err := s.Draw(ctx, "ok", WithIdempotencyKey("a:b")); return err }(),
		func() error { _, err := s.Pick(ctx, "ok", nil); return err }(),
		s.Disable(ctx, "ok", "bad id"),
		s.Add(ctx, "ok", Candidate{ID: "a", Weight: 0}),
		s.Add(ctx, "ok", Candidate{ID: "a", Weight: 1, Ext: map[string]string{"k": strings.Repeat("x", 5000)}}),
		s.SetWeight(ctx, "ok", "a", 1_000_001),
		s.Init(ctx, "ok", Spec{Mode: "other", Candidates: []Candidate{{ID: "a", Weight: 1}}}),
		s.Init(ctx, "ok", Spec{Mode: ModeSupplier}),
		s.Init(ctx, "ok", Spec{Mode: ModeSupplier, Candidates: []Candidate{{ID: "a", Weight: 1}, {ID: "a", Weight: 2}}}),
		s.Init(ctx, "ok", Spec{Mode: ModeLottery, Candidates: []Candidate{{ID: "a", Weight: 1, Stock: -2}}}),
	}
	for i, err := range calls {
		if err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
	if f.shaCalls+f.evalCalls != 0 {
		t.Fatalf("invalid input reached redis %d times", f.shaCalls+f.evalCalls)
	}
}

func TestSupplierMinActiveCannotBeZero(t *testing.T) {
	f := &fakeRedis{}
	s := mustNew(t, f)
	zero := int64(0)
	err := s.Init(context.Background(), "pay", Spec{
		Mode: ModeSupplier, MinActive: &zero, Candidates: []Candidate{{ID: "a", Weight: 1}},
	})
	if !errors.Is(err, ErrInvalidArgument) || f.shaCalls != 0 {
		t.Fatalf("got %v, calls %d", err, f.shaCalls)
	}
}

func TestNoScriptFallsBackToEval(t *testing.T) {
	f := &fakeRedis{
		shaErr: errors.New("NOSCRIPT No matching script. Please use EVAL."),
		reply:  []any{int64(1), "a", int64(70), int64(3)},
	}
	s := mustNew(t, f)
	res, err := s.Pick(context.Background(), "pay")
	if err != nil {
		t.Fatal(err)
	}
	if f.shaCalls != 1 || f.evalCalls != 1 {
		t.Fatalf("sha=%d eval=%d, want 1 and 1", f.shaCalls, f.evalCalls)
	}
	sum := sha1.Sum([]byte(f.lastSrc))
	if hex.EncodeToString(sum[:]) != f.lastSha {
		t.Fatal("EVALSHA digest does not match the script sent by EVAL")
	}
	if res.CandidateID != "a" || res.Weight != 70 || res.Version != 3 || res.RemainStock != -1 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestTransportErrorIsWrappedWithoutFallback(t *testing.T) {
	f := &fakeRedis{shaErr: context.DeadlineExceeded}
	s := mustNew(t, f)
	_, err := s.Pick(context.Background(), "pay")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if f.evalCalls != 0 {
		t.Fatal("a timeout must not trigger EVAL")
	}
}
