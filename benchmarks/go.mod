// Nested module: isolates the benchmark harness (a standalone main package)
// from the library's go.mod so it is NOT part of `go list ./...` and never
// affects the coverage floor. See BENCHMARKS.md.
module github.com/go-filesystems/fat32/benchmarks

go 1.27.4

require (
	github.com/go-filesystems/fat32 v0.5.1
	github.com/go-filesystems/interface v0.6.0
)

require (
	github.com/go-volumes/gpt v0.3.0 // indirect
	github.com/go-volumes/safeio v0.1.0 // indirect
)

replace github.com/go-filesystems/fat32 => ..
