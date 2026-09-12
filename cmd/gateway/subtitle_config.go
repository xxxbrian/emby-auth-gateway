package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
)

// Invalid optional subtitle tuning disables this module at the composition
// root; it must not stop the established proxy/audio services from starting.
func subtitleConfig(lookup func(string) (string, bool), bootID string) (subtitles.Config, bool, error) {
	cfg := subtitles.Config{BootID: bootID, Workers: 1, PrepareTimeout: 1500 * time.Millisecond,
		WorkTimeout: 30 * time.Second, MaxReadBytes: 32 << 20, MaxRequests: 512,
		CacheBytes: 128 << 20, SourceCacheBytes: 256 << 20}
	raw, exists := lookup("GATEWAY_WEB_SUBTITLES_ENABLED")
	if !exists {
		return cfg, false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return cfg, false, fmt.Errorf("GATEWAY_WEB_SUBTITLES_ENABLED must be a boolean")
	}
	if !enabled {
		return cfg, false, nil
	}
	cfg.Dir = filepath.Join(os.TempDir(), "emby-gateway-subtitles")
	if dir, exists := lookup("GATEWAY_SUBTITLE_CACHE_DIR"); exists {
		if strings.TrimSpace(dir) == "" {
			return cfg, true, fmt.Errorf("GATEWAY_SUBTITLE_CACHE_DIR must not be empty")
		}
		cfg.Dir = dir
	}
	for name, target := range map[string]*int64{
		"GATEWAY_SUBTITLE_CACHE_BUDGET":        &cfg.CacheBytes,
		"GATEWAY_SUBTITLE_SOURCE_CACHE_BUDGET": &cfg.SourceCacheBytes,
	} {
		if raw, exists := lookup(name); exists {
			number, multiplier, valid := splitStrictByteQuantity(raw)
			bytes, productValid := multiplyByteQuantity(number, multiplier)
			if !valid || !productValid || bytes < 1<<20 || bytes > 16<<30 {
				return cfg, true, fmt.Errorf("%s must be between 1MiB and 16GiB", name)
			}
			*target = int64(bytes)
		}
	}
	return cfg, true, nil
}
