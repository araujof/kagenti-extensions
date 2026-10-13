package main

import (
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/core/config"
)

// fallbackDefaultAddr is where the forward proxy is pinned for a local install. It is
// the answer whenever the config cannot give one, which is not a rare path: a config
// that will not load is one of the reasons the proxy cannot start, and that is exactly
// when the fallback has something to say. Kept equal to the default in
// cmd/agentop/setup_step_service.go, which picks it for the same reason.
const fallbackDefaultAddr = "127.0.0.1:47600"

// configPathFromArgs finds the -config value in the arguments the supervisor is about
// to hand the child. main.go declares it with flag.String, so all four spellings are
// accepted and all four are read here.
func configPathFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}

// supervisorForwardAddr is the address the fallback listener binds: the forward proxy's
// own, so a client routed at it reaches the fallback when no child holds it. Every miss
// falls back rather than failing, for the reason on fallbackDefaultAddr.
func supervisorForwardAddr(args []string) string {
	path := configPathFromArgs(args)
	if path == "" {
		return fallbackDefaultAddr
	}
	cfg, err := config.Load(path)
	if err != nil {
		slog.Debug("supervisor: cannot read the config for the fallback address; using the default",
			"config", path, "addr", fallbackDefaultAddr, "error", err)
		return fallbackDefaultAddr
	}
	if cfg.Listener.ForwardProxyAddr == "" {
		return fallbackDefaultAddr
	}
	return cfg.Listener.ForwardProxyAddr
}
