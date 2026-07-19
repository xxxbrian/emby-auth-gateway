//go:build !darwin && !linux

package main

import "errors"

func physicalMemoryBytes() (uint64, error) {
	return 0, errors.New("physical memory discovery is unsupported on this platform")
}
