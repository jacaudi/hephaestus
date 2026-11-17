//go:build !linux

package toolcache

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskFree is the default statfs: the bytes free to an unprivileged process on
// the volume holding path. Darwin has no f_frsize; its block counts are in units
// of f_bsize, the fundamental block size (statfs(2)).
func diskFree(path string) (free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Bsize == 0 {
		return 0, fmt.Errorf("statfs %s: block size 0", path)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
