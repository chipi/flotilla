package hostexec

import "syscall"

// statfs is the filesystem's available and total bytes at path, as the platform's statfs reports them (linux and
// darwin both have Bavail, Blocks and Bsize; the integer widths differ, hence the conversions).
func statfs(path string) (avail, size uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}
