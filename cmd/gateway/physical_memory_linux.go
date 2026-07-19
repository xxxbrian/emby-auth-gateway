//go:build linux

package main

import (
	"golang.org/x/sys/unix"
)

func physicalMemoryBytes() (uint64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	unit := uint64(info.Unit)
	if unit == 0 {
		unit = 1
	}
	return checkedPhysicalMemoryBytes(uint64(info.Totalram), unit)
}
