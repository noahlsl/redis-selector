# redis-selector

Redis 向けの分散重み付きセレクタ。重みに応じて下流サプライヤを選ぶか、抽選して在庫を原子的に減算します。

[English](README.md) | [简体中文](README.zh-CN.md) | [日本語](README.ja.md) | [한국어](README.ko.md)

## 機能

- **抽選モード**: Redis Lua で重み付き抽選と在庫減算を原子実行。任意の冪等キー対応
- **サプライヤモード**: 下流プロバイダを重み付きでランダム選択。追加 / 停止 / 有効化 / 削除 / 重み変更
- **最終アクティブ保護**: 最後の 1 件を停止・削除すると `ErrLastActiveProtected`
- **任意の KeyPrefix**: 環境やプロダクト線を分離しつつ Cluster のハッシュタグを維持
- **薄い Redis インターフェース**: Eval / EvalSha のみ。go-redis v9 / v8、go-zero、GoFrame gredis 対応
- Standalone / Cluster 対応: 同一セレクタのキーは同一ハッシュタグで 1 スロットに集約

## インストール

```bash
go get github.com/noahlsl/redis-selector
go get github.com/noahlsl/redis-selector/adapter/goredisv9
```

アダプタは独立モジュールです。使わない Redis クライアントは依存に入りません。

## クイックスタート（サプライヤ）

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
	// 設定なしでも可：Namespace はデフォルト "default"、KeyPrefix は空。
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

## クイックスタート（抽選）

```go
res, err := sel.Draw(ctx, "spring-2026",
	selector.WithIdempotencyKey("order-1001"),
)
// 限定賞品は Lua 内で在庫減算されます。
// 同じ冪等キーで再試行すると初回結果を返します。
```

## サンプル

ローカル Redis（127.0.0.1:6379）が必要です。REDIS_ADDR で上書きできます。

```bash
cd example
go mod tidy
go run ./supplier
go run ./lottery
go run ./observability
```

`./observability` は観測性の 2 層を示します：

- `WithObserver`: スクリプト単位の metrics/ログと、現在の span への属性付与
- `TracingPicker` / `TracingDrawer`: ビジネス呼び出し単位の span（コアに OpenTelemetry を組み込まない）

## アダプタ

| Adapter | Module | Client |
| --- | --- | --- |
| goredisv9 | adapter/goredisv9 | github.com/redis/go-redis/v9 |
| goredisv8 | adapter/goredisv8 | github.com/go-redis/redis/v8 |
| gozero | adapter/gozero | github.com/zeromicro/go-zero Redis store |
| goframe | adapter/goframe | GoFrame gredis (github.com/gogf/gf/v2) |

Standalone / Cluster クライアントのどちらを渡しても構いません。

## キー構成

```text
[<KeyPrefix>]{rs:<namespace>:<selector_id>}:<suffix>
```

KeyPrefix と Namespace は生成時のみ設定します。同一セレクタのキーは同じハッシュタグを共有し、Cluster では 1 スロットに集まります。

## テスト

ユニットテストは依存なしで実行できます。`REDIS_ADDR` に書き込み可能な Redis を指定すると結合テストも実行されます。各テストは専用のキープレフィックスを使い、自分が書いたキーだけを削除します。

```bash
go test ./... -race
REDIS_ADDR=127.0.0.1:6379 go test ./... -race
cd example && REDIS_ADDR=127.0.0.1:6379 go test ./...
```

## ドキュメント

- 機能設計書（英語版）: [docs/functional-design.md](docs/functional-design.md)
- 安全性・ACL・冪等・拡張点は同ドキュメント 9–10 章

## 要件

- コアモジュールは Go 1.18+。各アダプタの最低 Go バージョンは対応する Redis クライアントに従います
- Redis 5.0+（Standalone または Cluster）

## ライセンス

[MIT](LICENSE)
