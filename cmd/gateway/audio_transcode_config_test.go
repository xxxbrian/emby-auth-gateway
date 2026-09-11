package main

import "testing"

func TestAudioTranscodeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		env              map[string]string
		enabled, invalid bool
	}{
		{"disabled", map[string]string{}, false, false},
		{"disabled ignores tuning", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": "false", "GATEWAY_AUDIO_CACHE_BUDGET": "bad"}, false, false},
		{"defaults", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": "true"}, true, false},
		{"explicit", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": "true", "GATEWAY_AUDIO_CACHE_BUDGET": "2GiB", "GATEWAY_AUDIO_TRANSCODING_WORKERS": "8"}, true, false},
		{"empty enablement", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": ""}, false, true},
		{"small cache", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": "true", "GATEWAY_AUDIO_CACHE_BUDGET": "1MiB"}, false, true},
		{"bad worker limit", map[string]string{"GATEWAY_AUDIO_TRANSCODING_ENABLED": "true", "GATEWAY_AUDIO_TRANSCODING_WORKERS": "0"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, enabled, err := audioTranscodeConfig(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }, "boot")
			if (err != nil) != tc.invalid || enabled != tc.enabled {
				t.Fatalf("config %+v enabled=%v error=%v", cfg, enabled, err)
			}
		})
	}
}
