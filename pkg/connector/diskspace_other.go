//go:build !darwin && !linux

package connector

// freeDiskBytes can't be determined on this platform; return (0, false) so the
// caller skips the free-space guard rather than blocking downloads.
func freeDiskBytes(_ string) (uint64, bool) { return 0, false }
