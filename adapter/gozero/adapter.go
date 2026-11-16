// Package gozero adapts github.com/zeromicro/go-zero/core/stores/redis.
// Package gozero 适配 github.com/zeromicro/go-zero/core/stores/redis。
package gozero

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Adapter wraps *redis.Redis. Standalone or cluster is decided by RedisConf.Type (node / cluster); the adapter does not care.
// Adapter 包装 *redis.Redis。单点与集群由 RedisConf.Type（node / cluster）决定，适配器无需区分。
type Adapter struct{ r *redis.Redis }

func New(r *redis.Redis) *Adapter { return &Adapter{r: r} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.r.EvalCtx(ctx, script, keys, args...)
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.r.EvalShaCtx(ctx, sha1, keys, args...)
}
