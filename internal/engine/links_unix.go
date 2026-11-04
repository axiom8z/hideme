//go:build unix

package engine

import (
	"fmt"
	"os"
	"syscall"
)

// hardLinkCount says how many names the open file has.
func hardLinkCount(f *os.File) (uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no link count for %T", info.Sys())
	}
	return uint64(st.Nlink), nil
}
