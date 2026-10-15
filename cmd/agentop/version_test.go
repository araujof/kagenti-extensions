package main

import (
	"runtime/debug"
	"testing"
)

func TestDevVersion(t *testing.T) {
	const rev = "f256b3c2224042091cfe0ccd7f35f015373c760e"
	for _, tc := range []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{"no vcs stamp", nil, "dev"},
		{"clean tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}, "dev-f256b3c2"},
		{"dirty tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, "dev-f256b3c2+dirty"},
		{"modified without a revision", []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}, "dev"},
	} {
		if got := devVersion(tc.settings); got != tc.want {
			t.Errorf("%s: devVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
