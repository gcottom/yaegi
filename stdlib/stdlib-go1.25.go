//go:build go1.25

package stdlib

// This file declares generation directives for stdlib changes introduced in Go 1.25
// relative to Go 1.24. It intentionally contains only the differences between
// Go 1.24.0 and Go 1.25.1.
//
// When differences are identified (via pkg.go.dev and release notes), add
// go:generate directives here listing only the changed/added stdlib packages.
// Each directive should use ../internal/cmd/extract/extract as the generator.
//
// If there are no differences, this file intentionally contains no go:generate
// directives and will not produce any go1_25_* files.
