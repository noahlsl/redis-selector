// Package goredisv8 adapts github.com/go-redis/redis/v8. It is the same as the v9 adapter apart from the import path.
// Package goredisv8 适配 github.com/go-redis/redis/v8，写法与 v9 相同，只是导入路径不同。
package goredisv8

import (
	"context"

	"github.com/go-redis/redis/v8"
)

type Adapter struct{ c redis.Scripter }

func New(c redis.Scripter) *Adapter { return &Adapter{c: c} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.c.Eval(ctx, script, keys, args...).Result()
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.c.EvalSha(ctx, sha1, keys, args...).Result()
}
