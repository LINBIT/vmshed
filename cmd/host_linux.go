package cmd

import (
	"golang.org/x/sys/unix"
)

// hostMemory discovers the memory of the host as the default memory limit,
// rounded down to whole MiB. Limits imposed by cgroups, for example on a
// container, are not taken into account.
func hostMemory() (uint64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	memory := uint64(info.Totalram) * uint64(info.Unit)
	return memory - memory%(1<<20), nil
}
