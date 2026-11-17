//go:build linux

package toolcache

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskFree is the default statfs: the bytes free to an unprivileged process on
// the volume holding path. Linux counts the blocks of statfs(2) in units of
// f_frsize, the fragment size (statvfs(3) says the same); f_bsize is only the
// preferred I/O size and may differ, so Frsize is used and Bsize only when a
// kernel leaves Frsize zero.
func diskFree(path string) (free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	block := st.Frsize
	if block <= 0 {
		block = st.Bsize
	}
	if block <= 0 {
		return 0, fmt.Errorf("statfs %s: block size %d", path, block)
	}
	return st.Bavail * uint64(block), nil
}
