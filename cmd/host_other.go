//go:build !linux

package cmd

import "errors"

// hostMemory is only implemented for Linux, where vmshed runs.
func hostMemory() (uint64, error) {
	return 0, errors.New("host memory discovery is not supported on this platform")
}
