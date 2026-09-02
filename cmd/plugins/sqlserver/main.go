// Command fleetward-plugin-sqlserver serves the Microsoft SQL Server engine plugin.
//
// It is deliberately three lines. Everything the plugin does lives in plugins/sqlserver, so the
// conformance suite can exercise the engine without spawning a process.
package main

import (
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/plugins/sqlserver"
)

func main() {
	sdk.Serve(sqlserver.New())
}
