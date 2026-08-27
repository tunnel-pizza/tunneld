module github.com/cnuss/golib

// 1.24 is the floor for generic type aliases (`type BuilderV1[T any] =
// v1.Builder[T]` in lib.go) and for slog.DiscardHandler (v1alpha1's silent
// default logger). A library's `go` directive is its compatibility promise,
// so raise it only when the surface actually needs the newer semantics.
go 1.24
