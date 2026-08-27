// Package version exposes the build identity and the frozen /api/v1 contract
// version. External integrations pin behaviour to APIVersion, not to the
// application release, so a normal release never breaks a working client.
package version

import (
	"runtime/debug"
	"sync"
)

// Name identifies this service in API responses.
const Name = "rclone-syncd"

// APIVersion is the contract version of /api/v1. Bump the major only when an
// existing request or response shape changes in a way clients cannot ignore.
const APIVersion = "1.0.0"

// AppVersion can be stamped at build time with
// -ldflags "-X 115togd/internal/version.AppVersion=v1.2.3".
var AppVersion = "dev"

var (
	buildOnce sync.Once
	commit    string
	buildTime string
	modified  bool
)

func readBuildInfo() {
	buildOnce.Do(func() {
		commit = "unknown"
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					commit = setting.Value
				}
			case "vcs.time":
				buildTime = setting.Value
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	})
}

// Commit returns the VCS revision the binary was built from, or "unknown".
func Commit() string {
	readBuildInfo()
	if modified {
		return commit + "-dirty"
	}
	return commit
}

// BuildTime returns the VCS commit time, or an empty string when unavailable.
func BuildTime() string {
	readBuildInfo()
	return buildTime
}
