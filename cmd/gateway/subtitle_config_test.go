package main

import (
	"testing"
)

func TestSubtitleConfigDisabledIgnoresBudgets(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		values := map[string]string{"GATEWAY_SUBTITLE_CACHE_BUDGET": "invalid"}
		if flag != "" {
			values["GATEWAY_WEB_SUBTITLES_ENABLED"] = flag
		}
		_, enabled, err := subtitleConfig(func(key string) (string, bool) { v, ok := values[key]; return v, ok }, "boot")
		if enabled || err != nil {
			t.Fatalf("disabled module evaluated tuning: enabled=%v err=%v", enabled, err)
		}
	}
}

func TestSubtitleConfigBudgetsAndFailurePreserveRequestedMode(t *testing.T) {
	values := map[string]string{"GATEWAY_WEB_SUBTITLES_ENABLED": "true", "GATEWAY_SUBTITLE_CACHE_BUDGET": "32MiB", "GATEWAY_SUBTITLE_SOURCE_CACHE_BUDGET": "64MiB"}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, enabled, err := subtitleConfig(lookup, "boot")
	if err != nil || !enabled || cfg.CacheBytes != 32<<20 || cfg.SourceCacheBytes != 64<<20 || cfg.Workers != 1 {
		t.Fatalf("config: %+v %v %v", cfg, enabled, err)
	}
	values["GATEWAY_SUBTITLE_CACHE_BUDGET"] = "50GiB"
	_, enabled, err = subtitleConfig(lookup, "boot")
	if err == nil || !enabled {
		t.Fatal("invalid cache limits must retain the requested Web-filtering mode")
	}
}
