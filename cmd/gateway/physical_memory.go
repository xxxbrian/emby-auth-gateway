package main

import (
	"fmt"
	"math"
)

func checkedPhysicalMemoryBytes(total, unit uint64) (uint64, error) {
	if unit == 0 {
		unit = 1
	}
	if total > math.MaxUint64/unit {
		return 0, fmt.Errorf("physical memory size overflows uint64")
	}
	return total * unit, nil
}
