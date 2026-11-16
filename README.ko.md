# redis-selector

Redis 기반 분산 가중치 선택기입니다. 가중치로 하위 공급자를 고르거나, 추첨 후 재고를 원자적으로 차감합니다.

[English](README.md) | [简体中文](README.zh-CN.md) | [日本語](README.ja.md) | [한국어](README.ko.md)

## 기능

- **추첨 모드**: Redis Lua에서 가중치 추첨과 재고 차감을 원자 실행. 멱등 키 지원
- **공급자 모드**: 하위 공급자를 가중치로 무작위 선택. 추가 / 비활성 / 활성 / 삭제 / 가중치 변경
- **마지막 활성 보호**: 마지막 1개를 비활성·삭제하면 `ErrLastActiveProtected`
- **선택적 KeyPrefix**: 환경·제품 라인을 분리하면서 Cluster 해시 태그 유지
- **얇은 Redis 인터페이스**: Eval / EvalSha만 사용. go-redis v9 / v8, go-zero, GoFrame gredis 지원
- Standalone / Cluster 지원: 동일 선택기의 키는 같은 해시 태그로 한 슬롯에 모입니다

## 설치

```bash
go get github.com/noahlsl/redis-selector
go get github.com/noahlsl/redis-selector/adapter/goredisv9
```

어댑터는 독립 모듈이라 사용하지 않는 Redis 클라이언트는 의존성에 포함되지 않습니다.

## 빠른 시작 (공급자)

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
	// 설정 없이도 됩니다: Namespace 기본값은 "default", KeyPrefix는 비어 있습니다.
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

## 빠른 시작 (추첨)

```go
res, err := sel.Draw(ctx, "spring-2026",
	selector.WithIdempotencyKey("order-1001"),
)
// 한정 상품은 Lua에서 재고가 차감됩니다.
// 같은 멱등 키로 재시도하면 최초 결과를 반환합니다.
```

## 예제

로컬 Redis(127.0.0.1:6379)가 필요합니다. REDIS_ADDR로 변경할 수 있습니다.

```bash
cd example
go mod tidy
go run ./supplier
go run ./lottery
go run ./observability
```

`./observability` 는 관측성의 두 계층을 보여줍니다:

- `WithObserver`: 스크립트 단위 metrics/로그와 현재 span 속성 추가
- `TracingPicker` / `TracingDrawer`: 비즈니스 호출 단위 span(코어에 OpenTelemetry를 넣지 않음)

## 어댑터

| Adapter | Module | Client |
| --- | --- | --- |
| goredisv9 | adapter/goredisv9 | github.com/redis/go-redis/v9 |
| goredisv8 | adapter/goredisv8 | github.com/go-redis/redis/v8 |
| gozero | adapter/gozero | github.com/zeromicro/go-zero Redis store |
| goframe | adapter/goframe | GoFrame gredis (github.com/gogf/gf/v2) |

Standalone / Cluster 클라이언트 모두 전달할 수 있습니다.

## 키 레이아웃

```text
[<KeyPrefix>]{rs:<namespace>:<selector_id>}:<suffix>
```

KeyPrefix와 Namespace는 생성 시에만 설정합니다. 한 선택기의 모든 키는 같은 해시 태그를 공유해 Cluster에서 한 슬롯에 모입니다.

## 테스트

단위 테스트는 의존성 없이 실행됩니다. `REDIS_ADDR`에 쓰기 가능한 Redis를 지정하면 통합 테스트도 함께 실행됩니다. 각 테스트는 전용 키 접두사를 사용하고 자신이 쓴 키만 삭제합니다.

```bash
go test ./... -race
REDIS_ADDR=127.0.0.1:6379 go test ./... -race
cd example && REDIS_ADDR=127.0.0.1:6379 go test ./...
```

## 문서

- 기능 설계 문서(영어판): [docs/functional-design.md](docs/functional-design.md)
- 보안, ACL, 멱등, 확장 포인트는 해당 문서 9–10장

## 요구 사항

- 코어 모듈은 Go 1.18+. 각 어댑터의 최소 Go 버전은 해당 Redis 클라이언트를 따릅니다
- Redis 5.0+ (Standalone 또는 Cluster)

## 라이선스

[MIT](LICENSE)
