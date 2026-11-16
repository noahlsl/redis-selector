package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/noahlsl/redis-selector/adapter/goredisv9"
	"github.com/noahlsl/redis-selector/selector"
	"github.com/redis/go-redis/v9"
)

func main() {
	addr := env("REDIS_ADDR", "127.0.0.1:6379")
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	sel, err := selector.New(
		goredisv9.New(rdb),
		selector.WithKeyPrefix("demo"),
		selector.WithNamespace("supplier"),
	)
	if err != nil {
		log.Fatal(err)
	}

	const id = "payment"
	if err := sel.Init(ctx, id, selector.Spec{
		Mode:    selector.ModeSupplier,
		Replace: true,
		Candidates: []selector.Candidate{
			{ID: "alpha", Name: "Alpha Pay", Weight: 70},
			{ID: "beta", Name: "Beta Pay", Weight: 30},
		},
	}); err != nil {
		log.Fatalf("init: %v", err)
	}

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		res, err := sel.Pick(ctx, id)
		if err != nil {
			log.Fatalf("pick: %v", err)
		}
		counts[res.CandidateID]++
	}
	fmt.Println("weighted picks:", counts)

	if err := sel.Disable(ctx, id, "beta"); err != nil {
		log.Fatalf("disable beta: %v", err)
	}
	if err := sel.Disable(ctx, id, "alpha"); !errors.Is(err, selector.ErrLastActiveProtected) {
		log.Fatalf("expected last-active protection, got %v", err)
	}
	fmt.Println("last active protected: ok")

	res, err := sel.Pick(ctx, id)
	if err != nil {
		log.Fatalf("pick after disable: %v", err)
	}
	fmt.Println("only active supplier:", res.CandidateID)

	if err := sel.Enable(ctx, id, "beta"); err != nil {
		log.Fatalf("enable beta: %v", err)
	}
	fmt.Println("re-enabled beta")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
