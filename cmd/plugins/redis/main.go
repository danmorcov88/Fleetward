// Command fleetward-plugin-redis is the Fleetward engine plugin for Redis.
//
// It is launched and supervised by the control plane's plugin manager, not run directly. Running it
// from a shell prints go-plugin's handshake notice and exits, which is the intended behavior.
//
// This main deliberately contains no engine logic: everything lives in the plugins/redis package,
// so the plugin can be exercised by tests and by the conformance suite without spawning a process.
package main

import (
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/plugins/redis"
)

func main() {
	sdk.Serve(redis.New())
}
