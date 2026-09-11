package transcode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func sampleProcess(pid int) processSample {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	out := processSample{}
	if data, err := os.ReadFile(filepath.Join(base, "statm")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 1 {
			if pages, err := strconv.ParseInt(fields[1], 10, 64); err == nil && pages >= 0 {
				rss := pages * int64(os.Getpagesize())
				out.RSS = &rss
			}
		}
	}
	// schedstat reports actual CPU runtime in nanoseconds, avoiding a guessed
	// kernel clock-tick frequency. Include bounded FFmpeg thread activity.
	threads, err := os.ReadDir(filepath.Join(base, "task"))
	if err != nil || len(threads) > 128 {
		return out
	}
	var total float64
	for _, thread := range threads {
		data, err := os.ReadFile(filepath.Join(base, "task", thread.Name(), "schedstat"))
		if err != nil {
			return out
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return out
		}
		ns, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return out
		}
		total += float64(ns) / 1e9
	}
	if len(threads) > 0 {
		out.CPU = &total
	}
	return out
}
