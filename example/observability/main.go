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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	metrics := NewMetricsObserver()
	sel, err := selector.New(
		goredisv9.New(rdb),
		selector.WithKeyPrefix("demo"),
		selector.WithNamespace("observability"),
		selector.WithObserver(metrics.Observe),
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

	// Business code depends on Picker only; wrap it for request-level spans.
	// 业务侧只依赖 Picker；在外层包一层做请求级 span。
	picker := NewTracingPicker(sel, &LogTracer{})

	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		res, err := picker.Pick(ctx, id)
		if err != nil {
			log.Fatalf("pick: %v", err)
		}
		counts[res.CandidateID]++
	}
	fmt.Println("weighted picks:", counts)
	fmt.Println("observer snapshot:", metrics.Snapshot())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
