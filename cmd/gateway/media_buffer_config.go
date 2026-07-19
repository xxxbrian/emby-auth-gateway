package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

const (
	mediaBufferEnabledEnv = "GATEWAY_MEDIA_BUFFER_ENABLED"
	mediaBufferBudgetEnv  = "GATEWAY_MEDIA_BUFFER_BUDGET"
	goMemoryLimitEnv      = "GOMEMLIMIT"

	startupMediaBufferChunkSize = uint64(32 << 10)
	startupMediaBufferBudgetCap = uint64(2 << 30)
)

var errMediaBufferStartupConfig = errors.New("invalid media buffer startup configuration")

type mediaBufferStartupDeps struct {
	LookupEnv      func(string) (string, bool)
	ReadFile       func(string) ([]byte, error)
	PhysicalMemory func() (uint64, error)
}

func productionMediaBufferStartupDeps() mediaBufferStartupDeps {
	return mediaBufferStartupDeps{
		LookupEnv:      os.LookupEnv,
		ReadFile:       os.ReadFile,
		PhysicalMemory: physicalMemoryBytes,
	}
}

func injectMediaBufferStartup(deps mediaBufferStartupDeps, inject func(*gateway.MediaBuffer)) error {
	controller, err := resolveMediaBufferStartup(deps)
	if err != nil {
		return err
	}
	inject(controller)
	return nil
}

func resolveMediaBufferStartup(deps mediaBufferStartupDeps) (*gateway.MediaBuffer, error) {
	rawEnabled, present := deps.LookupEnv(mediaBufferEnabledEnv)
	if !present {
		return nil, nil
	}
	enabled, err := strconv.ParseBool(rawEnabled)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be a boolean: %v", errMediaBufferStartupConfig, mediaBufferEnabledEnv, err)
	}
	if !enabled {
		return nil, nil
	}

	if rawBudget, explicit := deps.LookupEnv(mediaBufferBudgetEnv); explicit {
		budget, parseErr := parseExplicitMediaBufferBudget(rawBudget)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %s: %v", errMediaBufferStartupConfig, mediaBufferBudgetEnv, parseErr)
		}
		return newStartupMediaBuffer(budget)
	}

	budget, err := automaticStartupMediaBufferBudget(deps)
	if err != nil {
		return nil, fmt.Errorf("%w: automatic budget: %v", errMediaBufferStartupConfig, err)
	}
	return newStartupMediaBuffer(budget)
}

func newStartupMediaBuffer(budget uint64) (*gateway.MediaBuffer, error) {
	if budget > math.MaxInt64 {
		return nil, fmt.Errorf("%w: budget overflows int64", errMediaBufferStartupConfig)
	}
	controller, err := gateway.NewMediaBuffer(int64(budget))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMediaBufferStartupConfig, err)
	}
	return controller, nil
}

func parseExplicitMediaBufferBudget(value string) (uint64, error) {
	if value == "" {
		return 0, errors.New("explicit value is empty")
	}
	number, multiplier, ok := splitStrictByteQuantity(value)
	if !ok {
		return 0, errors.New("must be a positive integer followed by B, KiB, MiB, or GiB")
	}
	bytes, ok := multiplyByteQuantity(number, multiplier)
	if !ok || bytes > math.MaxInt64 {
		return 0, errors.New("value overflows supported byte range")
	}
	if bytes == 0 {
		return 0, errors.New("value must be positive")
	}
	aligned := alignStartupMediaBufferSize(bytes)
	if aligned < startupMediaBufferChunkSize {
		return 0, fmt.Errorf("aligned budget must be at least %d bytes", startupMediaBufferChunkSize)
	}
	return aligned, nil
}

func splitStrictByteQuantity(value string) (uint64, uint64, bool) {
	var numberText string
	var multiplier uint64
	for _, suffix := range []struct {
		text       string
		multiplier uint64
	}{
		{text: "KiB", multiplier: 1 << 10},
		{text: "MiB", multiplier: 1 << 20},
		{text: "GiB", multiplier: 1 << 30},
		{text: "B", multiplier: 1},
	} {
		if strings.HasSuffix(value, suffix.text) {
			numberText = strings.TrimSuffix(value, suffix.text)
			multiplier = suffix.multiplier
			break
		}
	}
	if numberText == "" {
		return 0, 0, false
	}
	for _, digit := range numberText {
		if digit < '0' || digit > '9' {
			return 0, 0, false
		}
	}
	number, err := strconv.ParseUint(numberText, 10, 64)
	return number, multiplier, err == nil
}

func multiplyByteQuantity(number, multiplier uint64) (uint64, bool) {
	if multiplier == 0 || number > math.MaxUint64/multiplier {
		return 0, false
	}
	return number * multiplier, true
}

func automaticStartupMediaBufferBudget(deps mediaBufferStartupDeps) (uint64, error) {
	candidates := make([]uint64, 0, 5)
	for _, source := range cgroupMemorySources(deps.ReadFile) {
		if candidate, ok := readCgroupMemoryCandidate(deps.ReadFile, source.path, source.v1); ok {
			candidates = append(candidates, candidate)
		}
	}
	if raw, ok := deps.LookupEnv(goMemoryLimitEnv); ok {
		if candidate, valid := parseGoMemoryLimit(raw); valid {
			candidates = append(candidates, candidate)
		}
	}
	if candidate, err := deps.PhysicalMemory(); err == nil && candidate > 0 {
		candidates = append(candidates, candidate)
	}

	limit, ok := minimumPositiveUint64(candidates)
	if !ok {
		return 0, errors.New("no finite positive memory candidates")
	}
	budget := limit / 8
	if budget > startupMediaBufferBudgetCap {
		budget = startupMediaBufferBudgetCap
	}
	budget = alignStartupMediaBufferSize(budget)
	if budget < startupMediaBufferChunkSize {
		return 0, fmt.Errorf("aligned budget is below one %d-byte chunk", startupMediaBufferChunkSize)
	}
	return budget, nil
}

type cgroupMemorySource struct {
	path string
	v1   bool
}

type cgroupProcessPath struct {
	path string
	v1   bool
}

type cgroupMount struct {
	root       string
	mountpoint string
	v1         bool
}

func cgroupMemorySources(readFile func(string) ([]byte, error)) []cgroupMemorySource {
	var sources []cgroupMemorySource
	cgroupData, cgroupErr := readFile("/proc/self/cgroup")
	mountData, mountErr := readFile("/proc/self/mountinfo")
	if cgroupErr == nil && mountErr == nil {
		paths := parseCgroupProcessPaths(string(cgroupData))
		mounts := parseCgroupMounts(string(mountData))
		sources = resolveCgroupMemorySources(paths, mounts)
	}
	fallbacks := []cgroupMemorySource{
		{path: "/sys/fs/cgroup/memory.max"},
		{path: "/sys/fs/cgroup/memory/memory.limit_in_bytes", v1: true},
		{path: "/sys/fs/cgroup/memory.limit_in_bytes", v1: true},
	}
	seen := make(map[cgroupMemorySource]bool, len(sources)+len(fallbacks))
	result := make([]cgroupMemorySource, 0, len(sources)+len(fallbacks))
	for _, source := range append(sources, fallbacks...) {
		if seen[source] {
			continue
		}
		seen[source] = true
		result = append(result, source)
	}
	return result
}

func parseCgroupProcessPaths(data string) []cgroupProcessPath {
	var result []cgroupProcessPath
	for _, line := range strings.Split(data, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || !validAbsoluteCgroupPath(parts[2]) {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			result = append(result, cgroupProcessPath{path: path.Clean(parts[2])})
			continue
		}
		hierarchyID, err := strconv.ParseUint(parts[0], 10, 64)
		if err == nil && hierarchyID > 0 && commaListContains(parts[1], "memory") {
			result = append(result, cgroupProcessPath{path: path.Clean(parts[2]), v1: true})
		}
	}
	return result
}

func parseCgroupMounts(data string) []cgroupMount {
	var result []cgroupMount
	for _, line := range strings.Split(data, "\n") {
		sections := strings.SplitN(strings.TrimSpace(line), " - ", 2)
		if len(sections) != 2 {
			continue
		}
		before := strings.Fields(sections[0])
		after := strings.Fields(sections[1])
		if len(before) < 6 || len(after) < 3 || !validMountInfoIdentity(before[0], before[1], before[2]) {
			continue
		}
		root, rootOK := decodeMountInfoPath(before[3])
		mountpoint, mountOK := decodeMountInfoPath(before[4])
		if !rootOK || !mountOK || !validAbsoluteCgroupPath(root) || !validAbsoluteCgroupPath(mountpoint) {
			continue
		}
		switch after[0] {
		case "cgroup2":
			result = append(result, cgroupMount{root: path.Clean(root), mountpoint: path.Clean(mountpoint)})
		case "cgroup":
			if commaListContains(after[2], "memory") {
				result = append(result, cgroupMount{root: path.Clean(root), mountpoint: path.Clean(mountpoint), v1: true})
			}
		}
	}
	return result
}

func validMountInfoIdentity(mountID, parentID, device string) bool {
	if _, err := strconv.ParseUint(mountID, 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseUint(parentID, 10, 64); err != nil {
		return false
	}
	parts := strings.SplitN(device, ":", 2)
	if len(parts) != 2 {
		return false
	}
	_, majorErr := strconv.ParseUint(parts[0], 10, 64)
	_, minorErr := strconv.ParseUint(parts[1], 10, 64)
	return majorErr == nil && minorErr == nil
}

func resolveCgroupMemorySources(paths []cgroupProcessPath, mounts []cgroupMount) []cgroupMemorySource {
	seen := make(map[string]bool)
	var result []cgroupMemorySource
	for _, processPath := range paths {
		for _, mount := range mounts {
			if processPath.v1 != mount.v1 {
				continue
			}
			limitPath, ok := resolveCgroupLimitPath(processPath.path, mount)
			if !ok || seen[limitPath] {
				continue
			}
			seen[limitPath] = true
			result = append(result, cgroupMemorySource{path: limitPath, v1: processPath.v1})
		}
	}
	return result
}

func resolveCgroupLimitPath(processPath string, mount cgroupMount) (string, bool) {
	if !validAbsoluteCgroupPath(processPath) || !validAbsoluteCgroupPath(mount.root) || !validAbsoluteCgroupPath(mount.mountpoint) {
		return "", false
	}
	processPath = path.Clean(processPath)
	root := path.Clean(mount.root)
	mountpoint := path.Clean(mount.mountpoint)
	var relative string
	switch {
	case processPath == "/":
		relative = "."
	case root == "/":
		relative = strings.TrimPrefix(processPath, "/")
	case processPath == root:
		relative = "."
	case strings.HasPrefix(processPath, root+"/"):
		relative = strings.TrimPrefix(processPath, root+"/")
	default:
		relative = strings.TrimPrefix(processPath, "/")
	}
	filename := "memory.max"
	if mount.v1 {
		filename = "memory.limit_in_bytes"
	}
	resolved := path.Join(mountpoint, relative, filename)
	if !pathWithinMountpoint(resolved, mountpoint) {
		return "", false
	}
	return resolved, true
}

func pathWithinMountpoint(resolved, mountpoint string) bool {
	if mountpoint == "/" {
		return strings.HasPrefix(resolved, "/")
	}
	return resolved == mountpoint || strings.HasPrefix(resolved, mountpoint+"/")
}

func validAbsoluteCgroupPath(value string) bool {
	if !strings.HasPrefix(value, "/") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return false
		}
	}
	return true
}

func decodeMountInfoPath(value string) (string, bool) {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", false
		}
		digits := value[index+1 : index+4]
		parsed, err := strconv.ParseUint(digits, 8, 8)
		if err != nil {
			return "", false
		}
		decoded.WriteByte(byte(parsed))
		index += 3
	}
	return decoded.String(), true
}

func commaListContains(value, target string) bool {
	for _, item := range strings.Split(value, ",") {
		if item == target {
			return true
		}
	}
	return false
}

func readCgroupMemoryCandidate(readFile func(string) ([]byte, error), path string, v1 bool) (uint64, bool) {
	data, err := readFile(path)
	if err != nil {
		return 0, false
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "max" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 || v1 && isCgroupV1Unlimited(value) || value > math.MaxInt64 {
		return 0, false
	}
	return value, true
}

func isCgroupV1Unlimited(value uint64) bool {
	switch value {
	case math.MaxUint64,
		uint64(math.MaxInt64),
		uint64(math.MaxInt64) &^ uint64(4095),
		uint64(math.MaxInt64) &^ uint64(16383),
		uint64(math.MaxInt64) &^ uint64(65535),
		uint64(math.MaxInt32),
		uint64(math.MaxInt32) &^ uint64(4095),
		uint64(math.MaxInt32) &^ uint64(16383),
		uint64(math.MaxInt32) &^ uint64(65535):
		return true
	default:
		return false
	}
}

func parseGoMemoryLimit(value string) (uint64, bool) {
	if value == "" || value == "off" {
		return 0, false
	}
	for _, suffix := range []struct {
		text       string
		multiplier uint64
	}{
		{text: "TiB", multiplier: 1 << 40},
		{text: "GiB", multiplier: 1 << 30},
		{text: "MiB", multiplier: 1 << 20},
		{text: "KiB", multiplier: 1 << 10},
		{text: "B", multiplier: 1},
	} {
		if strings.HasSuffix(value, suffix.text) {
			numberText := strings.TrimSuffix(value, suffix.text)
			return parseUnsignedByteQuantity(numberText, suffix.multiplier)
		}
	}
	return parseUnsignedByteQuantity(value, 1)
}

func parseUnsignedByteQuantity(numberText string, multiplier uint64) (uint64, bool) {
	if numberText == "" {
		return 0, false
	}
	for _, digit := range numberText {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseUint(numberText, 10, 64)
	if err != nil || number == 0 {
		return 0, false
	}
	value, ok := multiplyByteQuantity(number, multiplier)
	return value, ok && value <= math.MaxInt64
}

func minimumPositiveUint64(candidates []uint64) (uint64, bool) {
	var minimum uint64
	for _, candidate := range candidates {
		if candidate > 0 && (minimum == 0 || candidate < minimum) {
			minimum = candidate
		}
	}
	return minimum, minimum > 0
}

func alignStartupMediaBufferSize(size uint64) uint64 {
	return size / startupMediaBufferChunkSize * startupMediaBufferChunkSize
}
