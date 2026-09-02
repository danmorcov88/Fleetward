//go:build windows

package manager

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// binarySuffix is the filename suffix a plugin binary carries on this platform. On Windows the
// extension is not cosmetic: it is the only thing that makes a file launchable.
const binarySuffix = ".exe"

// checkExecutable reports why the plugin binary at path cannot be launched, or nil if it can.
//
// Windows has no executable permission bit. os.Stat reports 0666 for every regular file, a compiled
// binary included, so the Unix check would reject every plugin on this platform. What actually
// decides launchability is the extension: exec.Command resolves through PATHEXT and refuses a file
// that does not carry one, so a plugin built without the suffix fails at launch with a far worse
// error than this one.
func checkExecutable(path string, _ fs.FileInfo) error {
	if !strings.EqualFold(filepath.Ext(path), binarySuffix) {
		return fmt.Errorf(
			"plugin manager: %q has no %s suffix and cannot be launched on Windows", path, binarySuffix)
	}
	return nil
}
