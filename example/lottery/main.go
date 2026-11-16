package main

import (
	"context"
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
		selector.WithNamespace("lottery"),
	)
	if err != nil {
		log.Fatal(err)
	}

	const id = "spring-2026"
	if err := sel.Init(ctx, id, selector.Spec{
		Mode:    selector.ModeLottery,
		Replace: true,
		Candidates: []selector.Candidate{
			{ID: "iphone", Name: "iPhone", Weight: 1, Stock: 2},
			{ID: "coupon", Name: "Coupon", Weight: 20, Stock: 10},
			{ID: "thanks", Name: "Thanks", Weight: 79, Stock: -1},
		},
	}); err != nil {
		log.Fatalf("init: %v", err)
	}

	first, err := sel.Draw(ctx, id, selector.WithIdempotencyKey("order-1001"))
	if err != nil {
		log.Fatalf("draw: %v", err)
	}
	replay, err := sel.Draw(ctx, id, selector.WithIdempotencyKey("order-1001"))
	if err != nil {
		log.Fatalf("replay: %v", err)
	}
	fmt.Printf("first=%s remain=%d replayed=%v\n", first.CandidateID, first.RemainStock, first.Replayed)
	fmt.Printf("replay=%s remain=%d replayed=%v\n", replay.CandidateID, replay.RemainStock, replay.Replayed)
	if !replay.Replayed || replay.CandidateID != first.CandidateID {
		log.Fatalf("idempotency failed")
	}

	snap, err := sel.Describe(ctx, id)
	if err != nil {
		log.Fatalf("describe: %v", err)
	}
	fmt.Printf("active=%d version=%d\n", snap.ActiveCount, snap.Version)
	for _, c := range snap.Candidates {
		fmt.Printf("  %s weight=%d stock=%d enabled=%v\n", c.ID, c.Weight, c.Stock, c.Enabled)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
