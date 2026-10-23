package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at release time by GoReleaser through -ldflags "-X main.version=...".
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionString reports the release-stamped version when there is one, and
// otherwise what the Go toolchain recorded: the module version for
// "go install ...@v0.1.0", and the VCS revision for a local "go build".
func versionString() string {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		modified := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if len(c) > 12 {
			c = c[:12]
		}
		if modified && c != "" && commit == "" {
			c += "-dirty"
		}
	}
	if v == "" {
		v = "dev"
	}
	if len(c) > 12 && commit != "" {
		c = c[:12]
	}
	s := "ragloop " + v
	if c != "" {
		s += " (" + c
		if d != "" {
			s += ", " + d
		}
		s += ")"
	}
	return fmt.Sprintf("%s %s/%s", s, runtime.GOOS, runtime.GOARCH)
}
