// Package version holds build-time version information injected via -ldflags.
package version

// These variables are overridden at link time:
//
//	-ldflags "-X github.com/nemvince/fos-next/internal/version.Version=v1.2.3
//	          -X github.com/nemvince/fos-next/internal/version.Commit=abc1234
//	          -X github.com/nemvince/fos-next/internal/version.BuildDate=2026-04-22"
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)
