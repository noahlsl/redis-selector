module github.com/noahlsl/redis-selector/example

go 1.26.0

require (
	github.com/noahlsl/redis-selector v0.0.0
	github.com/noahlsl/redis-selector/adapter/goredisv9 v0.0.0
	github.com/redis/go-redis/v9 v9.23.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/noahlsl/redis-selector => ../

replace github.com/noahlsl/redis-selector/adapter/goredisv9 => ../adapter/goredisv9
