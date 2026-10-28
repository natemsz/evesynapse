//go:build linux || darwin

package app

import "syscall"

// diskUsage reports the used and total bytes of the filesystem
// holding path (the host sample's disk figure), or zeros when it
// cannot be read.
func diskUsage(path string) (used, total int64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil || fs.Blocks == 0 {
		return 0, 0
	}
	total = int64(fs.Blocks) * int64(fs.Bsize)
	return total - int64(fs.Bfree)*int64(fs.Bsize), total
}
