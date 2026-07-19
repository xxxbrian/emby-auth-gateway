package telemetry

import (
	"encoding/json"
	"testing"
)

func TestSnapshotJSONAlwaysContainsFullZeroMediaBuffer(t *testing.T) {
	encoded, err := json.Marshal(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &root); err != nil {
		t.Fatal(err)
	}
	raw, ok := root["media_buffer"]
	if !ok {
		t.Fatalf("media_buffer missing from %s", encoded)
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"enabled":            false,
		"hard_budget_bytes":  float64(0),
		"allocated_bytes":    float64(0),
		"owned_bytes":        float64(0),
		"free_bytes":         float64(0),
		"active_requests":    float64(0),
		"base_only_requests": float64(0),
		"indebted_requests":  float64(0),
		"request_debt_bytes": float64(0),
	}
	if len(status) != len(want) {
		t.Fatalf("media_buffer fields=%v", status)
	}
	for key, value := range want {
		if status[key] != value {
			t.Fatalf("media_buffer[%q]=%v want %v", key, status[key], value)
		}
	}
}
