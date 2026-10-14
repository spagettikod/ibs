//go:build !linux

package blockstore

import "syscall"

// diskUsage returns the size of the filesystem holding dir and its space
// available to unprivileged users.
func diskUsage(dir string) (total, avail int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(uint64(st.Blocks) * uint64(st.Bsize)), int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
