// Package goredisv9 adapts github.com/redis/go-redis/v9.
// Package goredisv9 适配 github.com/redis/go-redis/v9。
package goredisv9

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Adapter accepts any client that implements redis.Scripter:
// *redis.Client (standalone), *redis.ClusterClient (cluster), redis.UniversalClient, *redis.Ring.
// Adapter 接受任何实现了 redis.Scripter 的客户端：
// *redis.Client（单点）、*redis.ClusterClient（集群）、redis.UniversalClient、*redis.Ring。
type Adapter struct{ c redis.Scripter }

func New(c redis.Scripter) *Adapter { return &Adapter{c: c} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.c.Eval(ctx, script, keys, args...).Result()
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.c.EvalSha(ctx, sha1, keys, args...).Result()
}
