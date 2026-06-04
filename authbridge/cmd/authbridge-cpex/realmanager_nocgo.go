//go:build cpex && !cgo

package main

import (
	"errors"

	cpexplugin "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/cpex"
)

// newRealManager is the CGO-disabled stub. The real implementation
// (in realmanager.go, gated by `cpex && cgo`) wraps the CPEX Go
// binding which is itself a CGO consumer of libcpex_ffi.a. Without
// CGO at compile time the binding can't link, so we surface a clear
// boot-time error instead of failing the package import.
//
// Production builds always set CGO_ENABLED=1; this stub exists only
// for local `go vet` / `go build -tags cpex CGO_ENABLED=0` runs that
// want to type-check the binary's main without pulling the FFI
// stack-up. The Dockerfile and CI both build with CGO=1.
func newRealManager() (cpexplugin.Manager, error) {
	return nil, errors.New(
		"authbridge-cpex was built with CGO_ENABLED=0; the cpex plugin " +
			"requires CGO + the linked libcpex_ffi.a — rebuild with " +
			"CGO_ENABLED=1 against a downloaded artifact (see CPEX_FFI_VERSION)")
}
