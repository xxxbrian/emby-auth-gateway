//go:build darwin || linux

package main

import "testing"

func TestPhysicalMemoryBytes(t *testing.T) {
	bytes, err := physicalMemoryBytes()
	if err != nil || bytes == 0 {
		t.Fatalf("physicalMemoryBytes()=%d,%v", bytes, err)
	}
}
