// Package version exposes build information stamped in at link time.
package version

import (
	"fmt"
	"runtime"
)

// ContractVersion is the version of the plugin contract this build speaks. It is reported by
// GetVersion and compared against every plugin's declared contract version at handshake time.
const ContractVersion = "v1"

// Values injected via -ldflags at build time. Defaults describe a local development build.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info describes this build.
type Info struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	Date            string `json:"date"`
	GoVersion       string `json:"go_version"`
	Platform        string `json:"platform"`
	ContractVersion string `json:"contract_version"`
}

// Get returns the build information for this binary.
func Get() Info {
	return Info{
		Version:         Version,
		Commit:          Commit,
		Date:            Date,
		GoVersion:       runtime.Version(),
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		ContractVersion: ContractVersion,
	}
}

// String renders the build information on a single line, suitable for `--version` output.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s, contract %s)",
		i.Version, i.Commit, i.Date, i.GoVersion, i.Platform, i.ContractVersion)
}
