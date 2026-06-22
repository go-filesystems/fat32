// Nested module: isolates the benchmark harness (a standalone main package)
// from the library's go.mod so it is NOT part of `go list ./...` and never
// affects the coverage floor. See BENCHMARKS.md.
module github.com/go-filesystems/fat32/benchmarks

go 1.26.4

require (
	github.com/go-filesystems/fat32 v0.0.0
	github.com/go-filesystems/interface v0.0.0-20260622072638-0b01d4fb163f
)

require (
	github.com/go-volumes/gpt v0.0.0-20260622072431-e1d6ba3b531c // indirect
	github.com/go-volumes/safeio v0.0.0-20260622072324-7f8eb19f6f8c // indirect
)

replace github.com/go-filesystems/fat32 => ..
