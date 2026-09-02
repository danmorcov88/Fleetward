//go:build !windows

package manager

import (
	"fmt"
	"io/fs"
)

// binarySuffix is the filename suffix a plugin binary carries on this platform. Unix binaries carry
// none; the executable bit is what marks them.
const binarySuffix = ""

// checkExecutable reports why the plugin binary at path cannot be launched, or nil if it can.
//
// Silently skipping a file that looks like a plugin but cannot run would present to an operator as
// "engine not supported", sending them hunting in the wrong place.
func checkExecutable(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("plugin manager: %q is not executable", path)
	}
	return nil
}
