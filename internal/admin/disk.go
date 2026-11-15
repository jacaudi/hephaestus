package admin

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

// DiskCheck returns an informational check of the free space on the
// filesystem holding path. It warns when the free fraction of the filesystem
// drops below minFreeRatio and fails when the filesystem cannot be read.
func DiskCheck(name, path string, minFreeRatio float64) Check {
	return Check{
		Name: name,
		Run: func(context.Context) Result {
			var st unix.Statfs_t
			if err := unix.Statfs(path, &st); err != nil {
				return Result{Status: StatusFail, Output: err.Error()}
			}
			if st.Blocks == 0 {
				return Result{Status: StatusFail, Output: "filesystem reports no blocks"}
			}
			free := float64(st.Bavail) / float64(st.Blocks) * 100
			res := Result{Status: StatusPass, ObservedValue: free, ObservedUnit: "%"}
			if free/100 < minFreeRatio {
				res.Status = StatusWarn
				res.Output = fmt.Sprintf("free space %.1f%% is below %.1f%%", free, minFreeRatio*100)
			}
			return res
		},
	}
}
