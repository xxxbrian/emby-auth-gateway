package main

import (
	"errors"
	"math"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func TestMediaBufferEnabledBoolean(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		present    bool
		wantEnable bool
		wantErr    bool
	}{
		{name: "absent defaults disabled"},
		{name: "false", value: "false", present: true},
		{name: "False", value: "False", present: true},
		{name: "FALSE", value: "FALSE", present: true},
		{name: "zero", value: "0", present: true},
		{name: "f", value: "f", present: true},
		{name: "F", value: "F", present: true},
		{name: "true", value: "true", present: true, wantEnable: true},
		{name: "True", value: "True", present: true, wantEnable: true},
		{name: "TRUE", value: "TRUE", present: true, wantEnable: true},
		{name: "one", value: "1", present: true, wantEnable: true},
		{name: "t", value: "t", present: true, wantEnable: true},
		{name: "T", value: "T", present: true, wantEnable: true},
		{name: "empty", value: "", present: true, wantErr: true},
		{name: "yes", value: "yes", present: true, wantErr: true},
		{name: "leading whitespace", value: " true", present: true, wantErr: true},
		{name: "trailing whitespace", value: "false ", present: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{mediaBufferBudgetEnv: "32KiB"}
			if tt.present {
				env[mediaBufferEnabledEnv] = tt.value
			}
			fake := newMediaBufferStartupFake(env)
			controller, err := resolveMediaBufferStartup(fake.deps())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && (controller != nil) != tt.wantEnable {
				t.Fatalf("controller=%v want enabled=%v", controller, tt.wantEnable)
			}
		})
	}
}

func TestParseExplicitMediaBufferBudget(t *testing.T) {
	tests := []struct {
		value   string
		want    uint64
		wantErr bool
	}{
		{value: "32768B", want: 32 << 10},
		{value: "32769B", want: 32 << 10},
		{value: "32KiB", want: 32 << 10},
		{value: "33KiB", want: 32 << 10},
		{value: "1MiB", want: 1 << 20},
		{value: "2GiB", want: 2 << 30},
		{value: "3GiB", want: 3 << 30},
		{value: "00032KiB", want: 32 << 10},
		{value: "", wantErr: true},
		{value: "0B", wantErr: true},
		{value: "32767B", wantErr: true},
		{value: "32", wantErr: true},
		{value: "1.5GiB", wantErr: true},
		{value: "+32KiB", wantErr: true},
		{value: "-32KiB", wantErr: true},
		{value: " 32KiB", wantErr: true},
		{value: "32 KiB", wantErr: true},
		{value: "32KiB ", wantErr: true},
		{value: "32KB", wantErr: true},
		{value: "32K", wantErr: true},
		{value: "32kib", wantErr: true},
		{value: "9223372036854775808B", wantErr: true},
		{value: "18446744073709551615GiB", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseExplicitMediaBufferBudget(tt.value)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseExplicitMediaBufferBudget(%q)=%d,%v want %d,error=%v", tt.value, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestMediaBufferDisabledIgnoresBudgetAndDiscovery(t *testing.T) {
	lookups := []string{}
	deps := mediaBufferStartupDeps{
		LookupEnv: func(name string) (string, bool) {
			lookups = append(lookups, name)
			if name != mediaBufferEnabledEnv {
				t.Fatalf("disabled mode looked up %s", name)
			}
			return "false", true
		},
		ReadFile: func(string) ([]byte, error) {
			t.Fatal("disabled mode read cgroup files")
			return nil, nil
		},
		PhysicalMemory: func() (uint64, error) {
			t.Fatal("disabled mode discovered physical memory")
			return 0, nil
		},
	}
	controller, err := resolveMediaBufferStartup(deps)
	if err != nil || controller != nil || !reflect.DeepEqual(lookups, []string{mediaBufferEnabledEnv}) {
		t.Fatalf("controller=%v error=%v lookups=%v", controller, err, lookups)
	}
}

func TestMediaBufferExplicitBudgetBypassesDiscovery(t *testing.T) {
	fake := newMediaBufferStartupFake(map[string]string{
		mediaBufferEnabledEnv: "true",
		mediaBufferBudgetEnv:  "2GiB",
		goMemoryLimitEnv:      "1B",
	})
	fake.failOnRead = true
	fake.failOnPhysical = true
	controller, err := resolveMediaBufferStartup(fake.deps())
	if err != nil || controller == nil {
		t.Fatalf("controller=%v error=%v", controller, err)
	}
	if !reflect.DeepEqual(fake.lookups, []string{mediaBufferEnabledEnv, mediaBufferBudgetEnv}) {
		t.Fatalf("lookups=%v", fake.lookups)
	}
}

func TestAutomaticMediaBufferCandidates(t *testing.T) {
	const (
		oneGiB   = uint64(1 << 30)
		eightGiB = uint64(8 << 30)
	)
	tests := []struct {
		name       string
		env        map[string]string
		files      map[string]string
		fileErrors map[string]error
		physical   uint64
		physErr    error
		want       uint64
		wantErr    bool
	}{
		{name: "cgroup v2 finite", files: map[string]string{"/sys/fs/cgroup/memory.max": "8GiB-invalid"}, wantErr: true},
		{name: "cgroup v2 bytes", files: map[string]string{"/sys/fs/cgroup/memory.max": "8589934592\n"}, want: oneGiB},
		{name: "process cgroup v2", files: map[string]string{
			"/proc/self/cgroup":    "0::/system.slice/emby.service\n",
			"/proc/self/mountinfo": "36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			"/sys/fs/cgroup/system.slice/emby.service/memory.max": "8589934592\n",
		}, want: oneGiB},
		{name: "resolved missing uses fallback", files: map[string]string{
			"/proc/self/cgroup":         "0::/service\n",
			"/proc/self/mountinfo":      "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/sys/fs/cgroup/memory.max": "8589934592\n",
		}, want: oneGiB},
		{name: "resolved unreadable uses fallback", files: map[string]string{
			"/proc/self/cgroup":         "0::/service\n",
			"/proc/self/mountinfo":      "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/sys/fs/cgroup/memory.max": "8589934592\n",
		}, fileErrors: map[string]error{"/run/cgroup/service/memory.max": os.ErrPermission}, want: oneGiB},
		{name: "resolved malformed uses fallback", files: map[string]string{
			"/proc/self/cgroup":              "0::/service\n",
			"/proc/self/mountinfo":           "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/run/cgroup/service/memory.max": "malformed\n",
			"/sys/fs/cgroup/memory.max":      "8589934592\n",
		}, want: oneGiB},
		{name: "tighter fallback wins minimum", files: map[string]string{
			"/proc/self/cgroup":              "0::/service\n",
			"/proc/self/mountinfo":           "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/run/cgroup/service/memory.max": "8589934592\n",
			"/sys/fs/cgroup/memory.max":      "4294967296\n",
		}, want: 512 << 20},
		{name: "v2 unlimited leaf finite parent", files: map[string]string{
			"/proc/self/cgroup":                  "0::/parent/leaf\n",
			"/proc/self/mountinfo":               "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/run/cgroup/parent/leaf/memory.max": "max\n",
			"/run/cgroup/parent/memory.max":      "4294967296\n",
			"/run/cgroup/memory.max":             "8589934592\n",
		}, want: 512 << 20},
		{name: "v1 unlimited leaf finite parent", files: map[string]string{
			"/proc/self/cgroup":                             "5:memory:/parent/leaf\n",
			"/proc/self/mountinfo":                          "31 25 0:26 / /run/memory rw - cgroup cgroup rw,memory\n",
			"/run/memory/parent/leaf/memory.limit_in_bytes": "9223372036854771712\n",
			"/run/memory/parent/memory.limit_in_bytes":      "8589934592\n",
			"/run/memory/memory.limit_in_bytes":             "17179869184\n",
		}, want: oneGiB},
		{name: "multiple finite ancestors choose tightest", files: map[string]string{
			"/proc/self/cgroup":                        "0::/grand/parent/leaf\n",
			"/proc/self/mountinfo":                     "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/run/cgroup/grand/parent/leaf/memory.max": "8589934592\n",
			"/run/cgroup/grand/parent/memory.max":      "2147483648\n",
			"/run/cgroup/grand/memory.max":             "4294967296\n",
			"/run/cgroup/memory.max":                   "17179869184\n",
		}, want: 256 << 20},
		{name: "bad ancestors omit independently", files: map[string]string{
			"/proc/self/cgroup":                  "0::/parent/leaf\n",
			"/proc/self/mountinfo":               "36 25 0:32 / /run/cgroup rw - cgroup2 cgroup rw\n",
			"/run/cgroup/parent/leaf/memory.max": "malformed\n",
			"/run/cgroup/memory.max":             "8589934592\n",
		}, fileErrors: map[string]error{"/run/cgroup/parent/memory.max": os.ErrPermission}, want: oneGiB},
		{name: "cgroup v2 max omitted", files: map[string]string{"/sys/fs/cgroup/memory.max": "max\n"}, physical: 16 << 30, want: 2 << 30},
		{name: "cgroup v1 nested finite", files: map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "8589934592"}, want: oneGiB},
		{name: "cgroup v1 root finite", files: map[string]string{"/sys/fs/cgroup/memory.limit_in_bytes": "8589934592"}, want: oneGiB},
		{name: "v1 long sentinel omitted", files: map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712"}, physical: eightGiB, want: oneGiB},
		{name: "v1 uint sentinel omitted", files: map[string]string{"/sys/fs/cgroup/memory.limit_in_bytes": "18446744073709551615"}, physical: eightGiB, want: oneGiB},
		{name: "malformed cgroups omitted", files: map[string]string{"/sys/fs/cgroup/memory.max": "bad", "/sys/fs/cgroup/memory/memory.limit_in_bytes": "-1"}, physical: eightGiB, want: oneGiB},
		{name: "gomemlimit finite", env: map[string]string{goMemoryLimitEnv: "8GiB"}, want: oneGiB},
		{name: "gomemlimit bytes without suffix", env: map[string]string{goMemoryLimitEnv: "8589934592"}, want: oneGiB},
		{name: "gomemlimit off omitted", env: map[string]string{goMemoryLimitEnv: "off"}, physical: eightGiB, want: oneGiB},
		{name: "gomemlimit malformed omitted", env: map[string]string{goMemoryLimitEnv: "8GB"}, physical: eightGiB, want: oneGiB},
		{name: "gomemlimit overflow omitted", env: map[string]string{goMemoryLimitEnv: "9223372036854775808B"}, physical: eightGiB, want: oneGiB},
		{name: "physical memory", physical: eightGiB, want: oneGiB},
		{name: "tightest candidate", env: map[string]string{goMemoryLimitEnv: "4GiB"}, files: map[string]string{"/sys/fs/cgroup/memory.max": "8589934592"}, physical: 16 << 30, want: 512 << 20},
		{name: "automatic cap", physical: math.MaxUint64, want: 2 << 30},
		{name: "missing and unreadable no candidate", physErr: errors.New("unavailable"), wantErr: true},
		{name: "subchunk", physical: 8*startupMediaBufferChunkSize - 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newMediaBufferStartupFake(tt.env)
			fake.files = tt.files
			fake.fileErrors = tt.fileErrors
			fake.physical = tt.physical
			fake.physicalErr = tt.physErr
			got, err := automaticStartupMediaBufferBudget(fake.deps())
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("budget=%d error=%v want=%d error=%v reads=%v", got, err, tt.want, tt.wantErr, fake.reads)
			}
		})
	}
}

func TestCgroupMemoryCandidateParsing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		v1    bool
		want  uint64
		ok    bool
	}{
		{name: "finite", value: "1048576\n", want: 1048576, ok: true},
		{name: "max", value: "max"},
		{name: "empty", value: ""},
		{name: "zero", value: "0"},
		{name: "malformed", value: "1MiB"},
		{name: "v1 sentinel", value: "9223372036854771712", v1: true},
		{name: "v1 finite one EiB", value: "1152921504606846976", v1: true, want: 1 << 60, ok: true},
		{name: "v1 finite near max", value: "9223372036854775806", v1: true, want: math.MaxInt64 - 1, ok: true},
		{name: "v2 same value finite", value: "9223372036854771712", want: 9223372036854771712, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := readCgroupMemoryCandidate(func(string) ([]byte, error) { return []byte(tt.value), nil }, "path", tt.v1)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("candidate=%d ok=%v want=%d,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := readCgroupMemoryCandidate(func(string) ([]byte, error) { return nil, os.ErrNotExist }, "path", false); ok {
		t.Fatal("missing cgroup file produced a candidate")
	}
	if _, ok := readCgroupMemoryCandidate(func(string) ([]byte, error) { return nil, os.ErrPermission }, "path", false); ok {
		t.Fatal("unreadable cgroup file produced a candidate")
	}
}

func TestCgroupMemorySources(t *testing.T) {
	fallback := []cgroupMemorySource{
		{path: "/sys/fs/cgroup/memory.max"},
		{path: "/sys/fs/cgroup/memory/memory.limit_in_bytes", v1: true},
		{path: "/sys/fs/cgroup/memory.limit_in_bytes", v1: true},
	}
	tests := []struct {
		name      string
		cgroup    string
		mountinfo string
		missing   string
		want      []cgroupMemorySource
	}{
		{
			name:      "systemd service v2",
			cgroup:    "0::/system.slice/emby-auth-gateway.service\n",
			mountinfo: "36 25 0:32 / /sys/fs/cgroup rw,nosuid,nodev - cgroup2 cgroup rw\n",
			want: []cgroupMemorySource{
				{path: "/sys/fs/cgroup/system.slice/emby-auth-gateway.service/memory.max"},
				{path: "/sys/fs/cgroup/system.slice/memory.max"},
			},
		},
		{
			name:      "container namespace root",
			cgroup:    "0::/\n",
			mountinfo: "36 25 0:32 /kubepods.slice/pod/container /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			want:      []cgroupMemorySource{{path: "/sys/fs/cgroup/memory.max"}},
		},
		{
			name:      "alternate escaped mountpoint and root",
			cgroup:    "0::/tenant slice/service.scope\n",
			mountinfo: "41 25 0:44 /tenant\\040slice /run/cgroup\\040v2 rw - cgroup2 cgroup rw\n",
			want: []cgroupMemorySource{
				{path: "/run/cgroup v2/service.scope/memory.max"},
				{path: "/run/cgroup v2/memory.max"},
			},
		},
		{
			name:   "v1 memory among multiple controllers",
			cgroup: "2:cpu,cpuacct:/docker/id\n5:memory,blkio:/docker/id\n",
			mountinfo: "30 25 0:25 / /sys/fs/cgroup/cpu rw - cgroup cgroup rw,cpu,cpuacct\n" +
				"31 25 0:26 /docker /alt/memory rw - cgroup cgroup rw,memory\n",
			want: []cgroupMemorySource{
				{path: "/alt/memory/id/memory.limit_in_bytes", v1: true},
				{path: "/alt/memory/memory.limit_in_bytes", v1: true},
			},
		},
		{
			name:      "duplicate mounts deduplicated",
			cgroup:    "0::/service\n",
			mountinfo: "36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n37 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			want:      []cgroupMemorySource{{path: "/sys/fs/cgroup/service/memory.max"}},
		},
		{
			name:      "root mountpoint",
			cgroup:    "0::/service\n",
			mountinfo: "36 25 0:32 / / rw - cgroup2 cgroup rw\n",
			want: []cgroupMemorySource{
				{path: "/service/memory.max"},
				{path: "/memory.max"},
			},
		},
		{
			name:      "malformed cgroup line omitted independently",
			cgroup:    "malformed\nnot-a-number:memory:/wrong\n0::/service\n",
			mountinfo: "36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			want:      []cgroupMemorySource{{path: "/sys/fs/cgroup/service/memory.max"}},
		},
		{
			name:      "malformed mount line omitted independently",
			cgroup:    "0::/service\n",
			mountinfo: "malformed\nbad 25 0:32 / /wrong rw - cgroup2 cgroup rw\n36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			want:      []cgroupMemorySource{{path: "/sys/fs/cgroup/service/memory.max"}},
		},
		{name: "missing cgroup proc falls back", missing: "/proc/self/cgroup", mountinfo: "valid", want: fallback},
		{name: "missing mountinfo falls back", missing: "/proc/self/mountinfo", cgroup: "0::/service", want: fallback},
		{name: "malformed cgroup falls back", cgroup: "malformed\n0::../../escape\n", mountinfo: "36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw", want: fallback},
		{name: "malformed mountinfo falls back", cgroup: "0::/service", mountinfo: "malformed", want: fallback},
		{name: "namespace relative descendant", cgroup: "0::/other/service", mountinfo: "36 25 0:32 /tenant /sys/fs/cgroup rw - cgroup2 cgroup rw", want: []cgroupMemorySource{
			{path: "/sys/fs/cgroup/other/service/memory.max"},
			{path: "/sys/fs/cgroup/other/memory.max"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{
				"/proc/self/cgroup":    tt.cgroup,
				"/proc/self/mountinfo": tt.mountinfo,
			}
			got := cgroupMemorySources(func(name string) ([]byte, error) {
				if name == tt.missing {
					return nil, os.ErrNotExist
				}
				return []byte(files[name]), nil
			})
			want := dedupeCgroupSources(append(append([]cgroupMemorySource{}, tt.want...), fallback...))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("sources=%#v want %#v", got, want)
			}
		})
	}
}

func TestResolveCgroupLimitPath(t *testing.T) {
	tests := []struct {
		name        string
		processPath string
		mount       cgroupMount
		want        string
		ok          bool
	}{
		{name: "v2 namespace descendant", processPath: "/child/grandchild", mount: cgroupMount{root: "/kubepods/pod/container", mountpoint: "/sys/fs/cgroup"}, want: "/sys/fs/cgroup/child/grandchild/memory.max", ok: true},
		{name: "v2 namespace root", processPath: "/", mount: cgroupMount{root: "/kubepods/pod/container", mountpoint: "/sys/fs/cgroup"}, want: "/sys/fs/cgroup/memory.max", ok: true},
		{name: "v2 host equal root", processPath: "/kubepods/pod/container", mount: cgroupMount{root: "/kubepods/pod/container", mountpoint: "/sys/fs/cgroup"}, want: "/sys/fs/cgroup/memory.max", ok: true},
		{name: "v2 host prefixed", processPath: "/kubepods/pod/container/child", mount: cgroupMount{root: "/kubepods/pod/container", mountpoint: "/sys/fs/cgroup"}, want: "/sys/fs/cgroup/child/memory.max", ok: true},
		{name: "v2 cleaned namespace path", processPath: "/child//grandchild", mount: cgroupMount{root: "/host/root", mountpoint: "/sys/fs/cgroup"}, want: "/sys/fs/cgroup/child/grandchild/memory.max", ok: true},
		{name: "v1 namespace descendant", processPath: "/child", mount: cgroupMount{root: "/docker/container", mountpoint: "/sys/fs/cgroup/memory", v1: true}, want: "/sys/fs/cgroup/memory/child/memory.limit_in_bytes", ok: true},
		{name: "v1 namespace root", processPath: "/", mount: cgroupMount{root: "/docker/container", mountpoint: "/sys/fs/cgroup/memory", v1: true}, want: "/sys/fs/cgroup/memory/memory.limit_in_bytes", ok: true},
		{name: "v1 host equal root", processPath: "/docker/container", mount: cgroupMount{root: "/docker/container", mountpoint: "/sys/fs/cgroup/memory", v1: true}, want: "/sys/fs/cgroup/memory/memory.limit_in_bytes", ok: true},
		{name: "v1 host prefixed", processPath: "/docker/container/child", mount: cgroupMount{root: "/docker/container", mountpoint: "/sys/fs/cgroup/memory", v1: true}, want: "/sys/fs/cgroup/memory/child/memory.limit_in_bytes", ok: true},
		{name: "relative process path rejected", processPath: "child", mount: cgroupMount{root: "/host", mountpoint: "/sys/fs/cgroup"}},
		{name: "process traversal rejected", processPath: "/child/../escape", mount: cgroupMount{root: "/host", mountpoint: "/sys/fs/cgroup"}},
		{name: "root traversal rejected", processPath: "/child", mount: cgroupMount{root: "/host/../escape", mountpoint: "/sys/fs/cgroup"}},
		{name: "mountpoint traversal rejected", processPath: "/child", mount: cgroupMount{root: "/host", mountpoint: "/sys/fs/cgroup/../escape"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveCgroupLimitPath(tt.processPath, tt.mount)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("resolveCgroupLimitPath()=%q,%v want %q,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestResolveCgroupLimitPathsIncludesVisibleAncestors(t *testing.T) {
	for _, tt := range []struct {
		name        string
		processPath string
		mount       cgroupMount
		want        []string
	}{
		{
			name:        "v2 namespace leaf to mountpoint",
			processPath: "/parent/leaf",
			mount:       cgroupMount{root: "/host/container", mountpoint: "/run/cgroup"},
			want:        []string{"/run/cgroup/parent/leaf/memory.max", "/run/cgroup/parent/memory.max", "/run/cgroup/memory.max"},
		},
		{
			name:        "v1 host prefixed leaf to mountpoint",
			processPath: "/docker/container/parent/leaf",
			mount:       cgroupMount{root: "/docker/container", mountpoint: "/run/memory", v1: true},
			want:        []string{"/run/memory/parent/leaf/memory.limit_in_bytes", "/run/memory/parent/memory.limit_in_bytes", "/run/memory/memory.limit_in_bytes"},
		},
		{
			name:        "namespace root stops at mountpoint",
			processPath: "/",
			mount:       cgroupMount{root: "/host/container", mountpoint: "/run/cgroup"},
			want:        []string{"/run/cgroup/memory.max"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveCgroupLimitPaths(tt.processPath, tt.mount)
			if !ok || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("paths=%v ok=%v want=%v", got, ok, tt.want)
			}
			for _, candidate := range got {
				if !pathWithinMountpoint(candidate, tt.mount.mountpoint) {
					t.Fatalf("candidate escaped mountpoint: %q", candidate)
				}
			}
		})
	}
}

func dedupeCgroupSources(sources []cgroupMemorySource) []cgroupMemorySource {
	seen := make(map[cgroupMemorySource]bool, len(sources))
	result := make([]cgroupMemorySource, 0, len(sources))
	for _, source := range sources {
		if seen[source] {
			continue
		}
		seen[source] = true
		result = append(result, source)
	}
	return result
}

func TestCgroupV1UnlimitedSentinels(t *testing.T) {
	sentinels := []uint64{
		math.MaxUint64,
		uint64(math.MaxInt64),
		uint64(math.MaxInt64) &^ uint64(4095),
		uint64(math.MaxInt64) &^ uint64(16383),
		uint64(math.MaxInt64) &^ uint64(65535),
		uint64(math.MaxInt32),
		uint64(math.MaxInt32) &^ uint64(4095),
		uint64(math.MaxInt32) &^ uint64(16383),
		uint64(math.MaxInt32) &^ uint64(65535),
	}
	for _, sentinel := range sentinels {
		t.Run(strconv.FormatUint(sentinel, 10), func(t *testing.T) {
			if !isCgroupV1Unlimited(sentinel) {
				t.Fatalf("sentinel %d not recognized", sentinel)
			}
			if _, ok := readCgroupMemoryCandidate(func(string) ([]byte, error) {
				return []byte(strconv.FormatUint(sentinel, 10)), nil
			}, "limit", true); ok {
				t.Fatalf("sentinel %d retained", sentinel)
			}
		})
	}
	for _, finite := range []uint64{1 << 60, (1 << 60) + 4096, uint64(math.MaxInt64) - 1} {
		if isCgroupV1Unlimited(finite) {
			t.Fatalf("finite value %d classified unlimited", finite)
		}
	}
}

func TestInjectMediaBufferStartupStopsBeforeServingAndInjectsController(t *testing.T) {
	failedInjectionCalled := false
	failure := newMediaBufferStartupFake(map[string]string{mediaBufferEnabledEnv: "invalid"})
	err := injectMediaBufferStartup(failure.deps(), func(*gateway.MediaBuffer) {
		failedInjectionCalled = true
	})
	if err == nil || failedInjectionCalled {
		t.Fatalf("error=%v injectionCalled=%v", err, failedInjectionCalled)
	}

	injected := false
	enabled := newMediaBufferStartupFake(map[string]string{mediaBufferEnabledEnv: "true", mediaBufferBudgetEnv: "32KiB"})
	if err := injectMediaBufferStartup(enabled.deps(), func(controller *gateway.MediaBuffer) {
		injected = controller != nil
	}); err != nil || !injected {
		t.Fatalf("error=%v injected=%v", err, injected)
	}

	disabled := newMediaBufferStartupFake(nil)
	if err := injectMediaBufferStartup(disabled.deps(), func(controller *gateway.MediaBuffer) {
		if controller != nil {
			t.Fatal("disabled startup injected a controller")
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredOnServeMediaBufferOrdering(t *testing.T) {
	originalDeps := mediaBufferStartupDepsForServe
	originalStartTelemetry := startTelemetryForServe
	originalNewGateway := newGatewayServerForServe
	originalMountGateway := mountGatewayRoutesForServe
	originalMountAdmin := mountAdminForServe
	originalBackground := startGatewayBackgroundForServe
	t.Cleanup(func() {
		mediaBufferStartupDepsForServe = originalDeps
		startTelemetryForServe = originalStartTelemetry
		newGatewayServerForServe = originalNewGateway
		mountGatewayRoutesForServe = originalMountGateway
		mountAdminForServe = originalMountAdmin
		startGatewayBackgroundForServe = originalBackground
	})

	var stages []string
	var constructedGateway *gateway.Server
	installServeOrderingSeams := func(fake *mediaBufferStartupFake, wantProvider bool) {
		constructedGateway = nil
		mediaBufferStartupDepsForServe = func() mediaBufferStartupDeps { return fake.deps() }
		startTelemetryForServe = func(registry *telemetry.Registry) {
			stages = append(stages, "telemetry-start")
			status := registry.MediaBufferAggregateSnapshot()
			if status.Enabled != wantProvider {
				t.Fatalf("telemetry started before provider wiring: %+v", status)
			}
		}
		newGatewayServerForServe = func(cfg gateway.Config, _ gateway.Store) *gateway.Server {
			stages = append(stages, "gateway-construction")
			if (cfg.MediaBuffer != nil) != wantProvider || cfg.MediaBufferLive == nil {
				t.Fatalf("gateway media buffer enabled=%v live=%v want provider=%v", cfg.MediaBuffer != nil, cfg.MediaBufferLive != nil, wantProvider)
			}
			constructedGateway = gateway.NewServer(cfg, gateway.NewMemoryStore())
			return constructedGateway
		}
		mountGatewayRoutesForServe = func(*router.Router[*core.RequestEvent], http.Handler, http.Handler, bool) {
			stages = append(stages, "gateway-routes")
		}
		mountAdminForServe = func(_ *router.Router[*core.RequestEvent], _ core.App, cfg adminConfig, _ *telemetry.Registry, mediaBufferSnapshot func() telemetry.MediaBufferStatus, _ func() bool, _ func(bool) (func(), error), _ bool, _ time.Time, _ string) error {
			stages = append(stages, "admin-routes")
			if mediaBufferSnapshot == nil || mediaBufferSnapshot().Enabled != wantProvider {
				t.Fatalf("admin media buffer enabled=%v want=%v", mediaBufferSnapshot != nil && mediaBufferSnapshot().Enabled, wantProvider)
			}
			if cfg.MediaBufferEnabled == nil || cfg.MediaBufferEnabled() != wantProvider {
				t.Fatalf("admin media buffer enabled=%v want=%v", cfg.MediaBufferEnabled != nil && cfg.MediaBufferEnabled(), wantProvider)
			}
			return nil
		}
		startGatewayBackgroundForServe = func(_ *core.ServeEvent, lifecycle *gatewayServerLifecycle) {
			stages = append(stages, "gateway-background")
			var current gatewayLifecycleServer
			if lifecycle != nil {
				current = lifecycle.Current()
			}
			if current != constructedGateway {
				t.Fatalf("background lifecycle current=%v want constructed gateway=%v", current, constructedGateway)
			}
		}
	}

	invalid := newMediaBufferStartupFake(map[string]string{mediaBufferEnabledEnv: "invalid"})
	installServeOrderingSeams(invalid, false)
	invalidNext := false
	invalidApp := newGatewayApp()
	invalidRouter, err := apis.NewRouter(invalidApp)
	if err != nil {
		t.Fatal(err)
	}
	err = invalidApp.OnServe().Trigger(&core.ServeEvent{App: invalidApp, Router: invalidRouter, Server: &http.Server{}}, func(*core.ServeEvent) error {
		invalidNext = true
		return nil
	})
	if err == nil || invalidNext || len(stages) != 0 {
		t.Fatalf("invalid startup error=%v next=%v stages=%v", err, invalidNext, stages)
	}

	stages = nil
	valid := newMediaBufferStartupFake(map[string]string{mediaBufferEnabledEnv: "true", mediaBufferBudgetEnv: "32KiB"})
	installServeOrderingSeams(valid, true)
	validApp := newGatewayApp()
	validRouter, err := apis.NewRouter(validApp)
	if err != nil {
		t.Fatal(err)
	}
	err = validApp.OnServe().Trigger(&core.ServeEvent{App: validApp, Router: validRouter, Server: &http.Server{}}, func(*core.ServeEvent) error {
		stages = append(stages, "next")
		return nil
	})
	wantStages := []string{"gateway-construction", "telemetry-start", "gateway-routes", "admin-routes", "gateway-background", "next"}
	if err != nil || !reflect.DeepEqual(stages, wantStages) {
		t.Fatalf("valid startup error=%v stages=%v want=%v", err, stages, wantStages)
	}

	stages = nil
	disabled := newMediaBufferStartupFake(nil)
	installServeOrderingSeams(disabled, false)
	disabledApp := newGatewayApp()
	disabledRouter, err := apis.NewRouter(disabledApp)
	if err != nil {
		t.Fatal(err)
	}
	err = disabledApp.OnServe().Trigger(&core.ServeEvent{App: disabledApp, Router: disabledRouter, Server: &http.Server{}}, func(*core.ServeEvent) error {
		stages = append(stages, "next")
		return nil
	})
	if err != nil || !reflect.DeepEqual(stages, wantStages) {
		t.Fatalf("disabled startup error=%v stages=%v want=%v", err, stages, wantStages)
	}
}

func TestCheckedPhysicalMemoryBytes(t *testing.T) {
	if got, err := checkedPhysicalMemoryBytes(1024, 4096); err != nil || got != 4<<20 {
		t.Fatalf("bytes=%d error=%v", got, err)
	}
	if got, err := checkedPhysicalMemoryBytes(1024, 0); err != nil || got != 1024 {
		t.Fatalf("zero-unit bytes=%d error=%v", got, err)
	}
	if _, err := checkedPhysicalMemoryBytes(math.MaxUint64, 2); err == nil {
		t.Fatal("physical memory multiplication overflow succeeded")
	}
}

type mediaBufferStartupFake struct {
	env            map[string]string
	files          map[string]string
	fileErrors     map[string]error
	physical       uint64
	physicalErr    error
	lookups        []string
	reads          []string
	failOnRead     bool
	failOnPhysical bool
}

func newMediaBufferStartupFake(env map[string]string) *mediaBufferStartupFake {
	return &mediaBufferStartupFake{env: env, physicalErr: errors.New("physical memory unavailable")}
}

func (f *mediaBufferStartupFake) deps() mediaBufferStartupDeps {
	return mediaBufferStartupDeps{
		LookupEnv: func(name string) (string, bool) {
			f.lookups = append(f.lookups, name)
			value, ok := f.env[name]
			return value, ok
		},
		ReadFile: func(path string) ([]byte, error) {
			if f.failOnRead {
				return nil, errors.New("unexpected cgroup discovery")
			}
			f.reads = append(f.reads, path)
			if err := f.fileErrors[path]; err != nil {
				return nil, err
			}
			value, ok := f.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(value), nil
		},
		PhysicalMemory: func() (uint64, error) {
			if f.failOnPhysical {
				return 0, errors.New("unexpected physical memory discovery")
			}
			return f.physical, f.physicalErr
		},
	}
}
