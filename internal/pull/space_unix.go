//go:build unix

package pull

import "syscall"

// FreeSpace is the number of bytes available to this user on the filesystem holding dir.
func FreeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // block counts fit in int64 on every supported platform
}
