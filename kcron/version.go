package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
// It falls back to "dev" for a plain `go build`/`go run`.
var Version = "dev"

// versionString combines the injected version with the VCS info that the Go
// toolchain embeds automatically, plus the toolchain and target platform.
func versionString() string {
	s := "KindleCron " + Version
	if revision, dirty := vcsInfo(); revision != "" {
		mark := ""
		if dirty {
			mark = "-dirty"
		}
		s += fmt.Sprintf(" (%s%s)", revision, mark)
	}
	s += fmt.Sprintf(", %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return s
}

func vcsInfo() (revision string, dirty bool) {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) > 12 {
				revision = setting.Value[:12]
			} else {
				revision = setting.Value
			}
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	return revision, dirty
}
