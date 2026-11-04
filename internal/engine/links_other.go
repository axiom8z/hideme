//go:build !unix && !windows

package engine

import "os"

// hardLinkCount reports one name where the platform has no way to say more.
func hardLinkCount(*os.File) (uint64, error) { return 1, nil }
