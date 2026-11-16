# redis-selector

Distributed weighted selector for Redis. Pick suppliers by weight, or draw lottery prizes with atomic stock deduction.

[English](README.md) | [简体中文](README.zh-CN.md) | [日本語](README.ja.md) | [한국어](README.ko.md)

## Features

- **Lottery mode**: weighted draw with atomic stock decrement in Redis Lua; optional idempotency key
- **Supplier mode**: weighted random selection among downstream providers; add / disable / enable / remove / set weight
- **Last-active protection**: refusing to disable or remove the only remaining supplier (`ErrLastActiveProtected`)
- **Optional KeyPrefix**: isolate environments and product lines without breaking Cluster hash tags
- **Thin Redis interface**: only Eval / EvalSha; adapters for go-redis v9, go-redis v8, go-zero, and GoFrame gredis
- Standalone and Cluster friendly: related keys share one hash tag so a script stays on a single slot

## Install

```bash
go get github.com/noahlsl/redis-selector
go get github.com/noahlsl/redis-selector/adapter/goredisv9
```

Adapters are separate modules so your project only pulls the Redis client you already use.

## Quick start (supplier)

```go
package main

import (
	"context"
	"time"

	"github.com/noahlsl/redis-selector/adapter/goredisv9"
	"github.com/noahlsl/redis-selector/selector"
	"github.com/redis/go-redis/v9"
)

func main() {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	// Zero-config is valid: Namespace defaults to "default", KeyPrefix is empty.
	// sel, _ := selector.New(goredisv9.New(rdb))
	sel, _ := selector.New(
		goredisv9.New(rdb),
		selector.WithKeyPrefix("demo"),
		selector.WithNamespace("supplier"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = sel.Init(ctx, "payment", selector.Spec{
		Mode:    selector.ModeSupplier,
		Replace: true,
		Candidates: []selector.Candidate{
			{ID: "alpha", Weight: 70},
			{ID: "beta", Weight: 30},
		},
	})

	res, _ := sel.Pick(ctx, "payment")
	println(res.CandidateID)
}
```

## Quick start (lottery)

```go
res, err := sel.Draw(ctx, "spring-2026",
	selector.WithIdempotencyKey("order-1001"),
)
// Limited prizes are decremented inside the Lua script.
// Retrying with the same idempotency key returns the first result.
```

## Examples

Requires a local Redis on 127.0.0.1:6379 (override with REDIS_ADDR).

```bash
cd example
go mod tidy
go run ./supplier
go run ./lottery
go run ./observability
```

`./observability` shows two observability layers:

- `WithObserver`: per-script metrics/logs, and script attributes on the current span
- `TracingPicker` / `TracingDrawer`: request-level spans without baking OpenTelemetry into the core package

## Adapters

| Adapter | Module | Client |
| --- | --- | --- |
| goredisv9 | adapter/goredisv9 | github.com/redis/go-redis/v9 |
| goredisv8 | adapter/goredisv8 | github.com/go-redis/redis/v8 |
| gozero | adapter/gozero | github.com/zeromicro/go-zero Redis store |
| goframe | adapter/goframe | GoFrame gredis (github.com/gogf/gf/v2) |

Pass a standalone or cluster client from the underlying library; the adapter does not care which mode it is.

## Key layout

```text
[<KeyPrefix>]{rs:<namespace>:<selector_id>}:<suffix>
```

KeyPrefix and Namespace are configured once at construction time. All keys for one selector share the same hash tag, so Cluster keeps them on one slot.

## Testing

Unit tests need nothing. Integration tests run when `REDIS_ADDR` points to a Redis you can write to; each test uses its own key prefix and removes only its own keys.

```bash
go test ./... -race
REDIS_ADDR=127.0.0.1:6379 go test ./... -race
cd example && REDIS_ADDR=127.0.0.1:6379 go test ./...
```

## Documentation

- Functional design document: [docs/functional-design.md](docs/functional-design.md)
- Safety, ACL, idempotency, and extension points are covered in sections 9-10 of that doc

## Requirements

- Go 1.18+ for the core module; each adapter follows the minimum Go version of its Redis client
- Redis 5.0+ (Standalone or Cluster)

## License

[MIT](LICENSE)
