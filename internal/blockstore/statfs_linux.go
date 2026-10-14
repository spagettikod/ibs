package blockstore

import "syscall"

// diskUsage returns the size of the filesystem holding dir and its space
// available to unprivileged users. Block counts are in fragment-size units
// (f_frsize), which can differ from the I/O block size, e.g. on virtiofs.
func diskUsage(dir string) (total, avail int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	unit := uint64(st.Frsize)
	if unit == 0 {
		unit = uint64(st.Bsize)
	}
	return int64(uint64(st.Blocks) * unit), int64(uint64(st.Bavail) * unit), nil
}
