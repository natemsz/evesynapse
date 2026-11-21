//go:build windows

package main

import "os"

// fileOwner has nothing to report on Windows, where a new file takes
// its permissions from the folder.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
