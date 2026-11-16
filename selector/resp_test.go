package selector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testRedis is a minimal RESP2 client for tests only, so the core module's integration tests need no third-party dependency.
// Each concurrent call uses its own connection; idle connections go back to the pool.
// testRedis 是只供测试使用的极简 RESP2 客户端，让核心模块的集成测试不引入第三方依赖。
// 每个并发调用使用独立连接，空闲连接放回池中复用。
type testRedis struct {
	network, addr string
	pool          chan *respConn
}

type respConn struct {
	c net.Conn
	r *bufio.Reader
}

// redisError is an error reply from Redis, as opposed to a network error.
// redisError 是 Redis 返回的错误回复，与网络错误区分开。
type redisError string

func (e redisError) Error() string { return string(e) }

func newTestRedis(addr string) *testRedis {
	network := "tcp"
	if strings.HasPrefix(addr, "/") {
		network = "unix"
	}
	return &testRedis{network: network, addr: addr, pool: make(chan *respConn, 64)}
}

func (t *testRedis) do(args ...any) (any, error) {
	var conn *respConn
	select {
	case conn = <-t.pool:
	default:
		c, err := net.DialTimeout(t.network, t.addr, 2*time.Second)
		if err != nil {
			return nil, err
		}
		conn = &respConn{c: c, r: bufio.NewReader(c)}
	}
	_ = conn.c.SetDeadline(time.Now().Add(10 * time.Second))
	v, err := conn.do(args...)
	if _, isReply := err.(redisError); err != nil && !isReply {
		conn.c.Close()
		return nil, err
	}
	select {
	case t.pool <- conn:
	default:
		conn.c.Close()
	}
	return v, err
}

func (t *testRedis) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	return t.do(evalArgs("EVAL", script, keys, args)...)
}

func (t *testRedis) EvalSha(_ context.Context, sha string, keys []string, args ...any) (any, error) {
	return t.do(evalArgs("EVALSHA", sha, keys, args)...)
}

func evalArgs(cmd, body string, keys []string, args []any) []any {
	out := []any{cmd, body, len(keys)}
	for _, k := range keys {
		out = append(out, k)
	}
	return append(out, args...)
}

func (c *respConn) do(args ...any) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		s := fmt.Sprint(a)
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(s), s)
	}
	if _, err := io.WriteString(c.c, b.String()); err != nil {
		return nil, err
	}
	return c.read()
}

func (c *respConn) read() (any, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, fmt.Errorf("empty reply")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, redisError(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = c.read(); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

// newIntegration connects to the Redis named by REDIS_ADDR (host:port or a unix socket path) and skips the test when it is unset.
// Each test gets its own KeyPrefix and removes every key it wrote when it ends.
// A trailing WithKeyPrefix is always appended, so any KeyPrefix in opts is overridden.
// newIntegration 连接 REDIS_ADDR 指定的 Redis（host:port 或 unix socket 路径），未设置时跳过。
// 每个测试使用独立的 KeyPrefix，结束时清理自己写入的全部 Key。
// 末尾会追加 WithKeyPrefix，opts 里的 KeyPrefix 会被覆盖。
func newIntegration(t *testing.T, opts ...ConfigOption) (Selector, *testRedis, string) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run integration tests")
	}
	rdb := newTestRedis(addr)
	if _, err := rdb.do("PING"); err != nil {
		t.Fatalf("redis %s unreachable: %v", addr, err)
	}
	prefix := fmt.Sprintf("rstest:%d-%d:", os.Getpid(), time.Now().UnixNano())
	opts = append(opts, WithKeyPrefix(prefix))
	sel, err := New(rdb, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keys, _ := rdb.do("KEYS", prefix+"*")
		for _, k := range toSlice(keys) {
			_, _ = rdb.do("DEL", k)
		}
	})
	return sel, rdb, prefix
}
