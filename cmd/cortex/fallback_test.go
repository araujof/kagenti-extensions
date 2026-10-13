package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The supervisor never parsed config before this, so it learns the address the same
// way it learns everything else: from the argv it is about to hand the child. Both
// spellings and both forms, because main.go uses flag.String and so accepts all four.
func TestConfigPathFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate single dash", []string{"-config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"separate double dash", []string{"--config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined single dash", []string{"-config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined double dash", []string{"--config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"among others", []string{"-local", "--config", "/etc/c.yaml", "-ca-dir", "/d"}, "/etc/c.yaml"},
		{"absent", []string{"-local"}, ""},
		{"dangling value", []string{"-local", "--config"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configPathFromArgs(tc.args); got != tc.want {
				t.Errorf("configPathFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// Every miss has to yield an address, not an error. The whole point of the fallback is
// to speak when the proxy cannot start, and "the config will not load" is one of the
// reasons it cannot start -- so that case must not be the one where we fall silent.
func TestSupervisorForwardAddr_FallsBackWhereverTheConfigCannotAnswer(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("mode: proxy-sidecar\nlistener:\n  forward_proxy_addr: 127.0.0.1:48123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("listener: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"config names an address", []string{"--config", good}, "127.0.0.1:48123"},
		{"config will not parse", []string{"--config", broken}, fallbackDefaultAddr},
		{"config is not there", []string{"--config", filepath.Join(dir, "absent.yaml")}, fallbackDefaultAddr},
		{"no config flag at all", []string{"-local"}, fallbackDefaultAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorForwardAddr(tc.args); got != tc.want {
				t.Errorf("supervisorForwardAddr(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
