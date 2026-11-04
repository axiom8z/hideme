package engine

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/axiom8z/hideme/internal/core"
)

// FilesystemLimit states what inspect can and cannot see on this platform.
const FilesystemLimit = "Extended attributes are reported, except security.* and system.*, which the operating system manages. Outputs are new files and inherit none of them; the original keeps its attributes unless replaced with --in-place."

// maxXattrBytes is the kernel's own limit on an attribute value or on a name list.
const maxXattrBytes = 64 * 1024

// downloadAttrs are the attributes browsers set on Linux, whose values are worth
// decoding because they hold the URL a file came from.
var downloadAttrs = map[string]string{
	"user.xdg.origin.url":   "Download URL",
	"user.xdg.referrer.url": "Download referrer URL",
}

// inspectFilesystem reports the file's extended attributes.
func inspectFilesystem(path string, rep *core.Report) {
	buf := make([]byte, maxXattrBytes)
	n, err := syscall.Listxattr(path, buf)
	if err != nil {
		if !errors.Is(err, syscall.ENOTSUP) {
			rep.Warn("could not list this file's extended attributes: %v", err)
		}
		return
	}
	value := make([]byte, maxXattrBytes)
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if name == "" || strings.HasPrefix(name, "security.") || strings.HasPrefix(name, "system.") {
			continue
		}
		size, err := syscall.Getxattr(path, name, value)
		if err != nil {
			rep.Warn("could not read the extended attribute %s: %v", name, err)
			continue
		}
		if field, ok := downloadAttrs[name]; ok {
			rep.Add(core.Finding{
				Category: core.CatFilesystem,
				Risk:     core.RiskHigh,
				Field:    field,
				Value:    string(value[:size]),
				Where:    "extended attribute " + name,
				Action:   core.ActionRemove,
			})
			continue
		}
		rep.Add(core.Finding{
			Category: core.CatFilesystem,
			Risk:     core.RiskMed,
			Field:    "Extended attribute",
			Value:    fmt.Sprintf("%s, %d bytes", name, size),
			Where:    "extended attribute", // the name can be the user's own, so it stays in the value
			Action:   core.ActionRemove,
		})
	}
}

// sourceTimes returns the access time from the stat structure. Linux offers no
// portable way to set a birth time, so there is none to carry over.
func sourceTimes(info os.FileInfo) (access, create time.Time) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)), time.Time{}
	}
	return info.ModTime(), time.Time{}
}
