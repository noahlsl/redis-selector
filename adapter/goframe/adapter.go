// Package goframe adapts GoFrame's github.com/gogf/gf/v2/database/gredis.
// Package goframe 适配 GoFrame 的 github.com/gogf/gf/v2/database/gredis。
package goframe

import (
	"context"

	"github.com/gogf/gf/v2/database/gredis"
)

// Adapter wraps *gredis.Redis (for example g.Redis()). With several addresses configured, the underlying client is a cluster client.
// gredis turns array replies into []string; the core package accepts that form.
// Adapter 包装 *gredis.Redis（例如 g.Redis()）。配置多个地址时底层是集群客户端。
// gredis 会把数组回复转成 []string，核心包已兼容这种形态。
type Adapter struct{ r *gredis.Redis }

func New(r *gredis.Redis) *Adapter { return &Adapter{r: r} }

func (a *Adapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.do(ctx, "EVAL", script, keys, args)
}

func (a *Adapter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) (any, error) {
	return a.do(ctx, "EVALSHA", sha1, keys, args)
}

func (a *Adapter) do(ctx context.Context, cmd, body string, keys []string, args []any) (any, error) {
	params := make([]any, 0, 2+len(keys)+len(args))
	params = append(params, body, len(keys))
	for _, k := range keys {
		params = append(params, k)
	}
	params = append(params, args...)
	v, err := a.r.Do(ctx, cmd, params...)
	if err != nil {
		return nil, err
	}
	return v.Val(), nil
}
