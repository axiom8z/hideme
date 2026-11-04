//go:build !windows && !linux

package engine

import (
	"os"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// FilesystemLimit states what inspect can and cannot see on this platform. The
// standard library's syscall package exposes no extended attribute calls here.
const FilesystemLimit = "Extended attributes, such as com.apple.metadata:kMDItemWhereFroms on macOS, cannot be read on this platform without third-party code, so inspect does not report them. Outputs are new files and inherit none of them; the original keeps its attributes unless replaced with --in-place."

func inspectFilesystem(string, *core.Report) {}

// sourceTimes has only the modification time to go on here.
func sourceTimes(info os.FileInfo) (access, create time.Time) {
	return info.ModTime(), time.Time{}
}
