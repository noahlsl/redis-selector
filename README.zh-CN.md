# redis-selector

基于 Redis 的分布式权重选择器：按权重选择下游供应商，或按权重抽奖并原子扣减库存。

[English](README.md) | [简体中文](README.zh-CN.md) | [日本語](README.ja.md) | [한국어](README.ko.md)

## 功能

- **抽奖模式**：按权重抽取，限量奖品在 Redis Lua 脚本内原子扣库存，支持幂等键
- **供应商模式**：按权重随机选择下游供应商，支持新增、停用、启用、删除、改权重
- **末位保护**：只剩一个可用供应商时，停用或删除会返回 `ErrLastActiveProtected`
- **可选 KeyPrefix**：隔离环境与业务线，不影响 Cluster Hash Tag
- **精简 Redis 接口**：只有 Eval / EvalSha；适配 go-redis v9、go-redis v8、go-zero、GoFrame gredis
- 兼容单点与集群：同一选择器的 Key 共享 Hash Tag，脚本落在同一 Slot

## 安装

```bash
go get github.com/noahlsl/redis-selector
go get github.com/noahlsl/redis-selector/adapter/goredisv9
```

适配器是独立模块，不会把你没用到的 Redis 客户端拉进依赖树。

## 快速开始（供应商）

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
	// 零配置可用：Namespace 默认 "default"，KeyPrefix 为空。
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

## 快速开始（抽奖）

```go
res, err := sel.Draw(ctx, "spring-2026",
	selector.WithIdempotencyKey("order-1001"),
)
// 限量奖品在 Lua 脚本内扣库存。
// 用同一个幂等键重试，会返回首次结果。
```

## 示例

需要本机 Redis：127.0.0.1:6379（可用 REDIS_ADDR 覆盖）。

```bash
cd example
go mod tidy
go run ./supplier
go run ./lottery
go run ./observability
```

`./observability` 演示了两层可观测：

- `WithObserver`：按脚本打 metrics/日志，并把脚本属性挂到当前 span
- `TracingPicker` / `TracingDrawer`：按业务调用开 span，不把 OpenTelemetry 写进核心库

## 适配器

| 适配器 | 模块 | 客户端 |
| --- | --- | --- |
| goredisv9 | adapter/goredisv9 | github.com/redis/go-redis/v9 |
| goredisv8 | adapter/goredisv8 | github.com/go-redis/redis/v8 |
| gozero | adapter/gozero | github.com/zeromicro/go-zero Redis store |
| goframe | adapter/goframe | GoFrame gredis (github.com/gogf/gf/v2) |

传入底层库的单点或集群客户端即可，适配器不区分模式。

## Key 布局

```text
[<KeyPrefix>]{rs:<namespace>:<selector_id>}:<suffix>
```

KeyPrefix 与 Namespace 只在构造时配置。同一选择器的全部 Key 共享 Hash Tag，Cluster 下落在同一 Slot。

## 测试

单元测试不需要任何依赖。设置 `REDIS_ADDR` 指向一个可写的 Redis 后会同时运行集成测试；每个测试使用独立的 Key 前缀，结束时只清理自己写入的 Key。

```bash
go test ./... -race
REDIS_ADDR=127.0.0.1:6379 go test ./... -race
cd example && REDIS_ADDR=127.0.0.1:6379 go test ./...
```

## 文档

- 功能设计文档（英文版）：[docs/functional-design.md](docs/functional-design.md)
- 安全设计、ACL、幂等与扩展点见该文档第 9–10 节

## 要求

- 核心模块要求 Go 1.18+；各适配器的最低 Go 版本跟随对应的 Redis 客户端
- Redis 5.0+（Standalone 或 Cluster）

## 许可证

[MIT](LICENSE)
