//go:build darwin

package main

import "golang.org/x/sys/unix"

func physicalMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
