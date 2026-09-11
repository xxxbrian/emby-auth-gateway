package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

func audioTranscodeConfig(lookup func(string) (string, bool), bootID string) (transcode.Config, bool, error) {
	cfg := transcode.Config{BootID: bootID}
	raw, ok := lookup("GATEWAY_AUDIO_TRANSCODING_ENABLED")
	if !ok {
		return cfg, false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return cfg, false, fmt.Errorf("GATEWAY_AUDIO_TRANSCODING_ENABLED must be a boolean")
	}
	if !enabled {
		return cfg, false, nil
	}
	cfg.CacheDir = filepath.Join(os.TempDir(), "emby-gateway-audio-cache")
	if value, ok := lookup("GATEWAY_AUDIO_CACHE_DIR"); ok {
		if strings.TrimSpace(value) == "" {
			return cfg, false, fmt.Errorf("GATEWAY_AUDIO_CACHE_DIR is empty")
		}
		cfg.CacheDir = value
	}
	if value, ok := lookup("GATEWAY_FFMPEG_PATH"); ok {
		if strings.TrimSpace(value) == "" {
			return cfg, false, fmt.Errorf("GATEWAY_FFMPEG_PATH is empty")
		}
		cfg.FFmpegPath = value
	}
	if value, ok := lookup("GATEWAY_AUDIO_TRANSCODING_WORKERS"); ok {
		cfg.Workers, err = strconv.Atoi(value)
		if err != nil || cfg.Workers < 1 || cfg.Workers > 32 {
			return cfg, false, fmt.Errorf("GATEWAY_AUDIO_TRANSCODING_WORKERS must be between 1 and 32")
		}
	}
	if value, ok := lookup("GATEWAY_AUDIO_CACHE_BUDGET"); ok {
		number, multiplier, valid := splitStrictByteQuantity(value)
		bytes, validProduct := multiplyByteQuantity(number, multiplier)
		if !valid || !validProduct || bytes < 16<<20 || bytes > 1<<50 {
			return cfg, false, fmt.Errorf("GATEWAY_AUDIO_CACHE_BUDGET must be between 16MiB and 1PiB, using B, KiB, MiB or GiB")
		}
		cfg.CacheBytes = int64(bytes)
	}
	return cfg, true, nil
}
