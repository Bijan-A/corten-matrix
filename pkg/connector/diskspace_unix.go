//go:build darwin || linux

package connector

import "golang.org/x/sys/unix"

// freeDiskBytes returns the bytes available to a non-root process on the
// filesystem backing path, and true on success. Used to gate attachment
// downloads-to-disk so a burst of concurrent large downloads can't fill the
// disk. Returns (0, false) if the filesystem can't be stat'd.
func freeDiskBytes(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}
