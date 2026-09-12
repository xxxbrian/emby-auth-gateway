package transcode

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestMediaSourceProvenanceJobSnapshotOwnershipAndImmutability(t *testing.T) {
	m := &Manager{ctx: context.Background(), cfg: Config{MaxJobs: 4}, jobs: map[string]*job{}, inputs: map[string]*job{}, cache: &catalog{}}
	identity := Identity{Owner: "secret-owner", ItemID: "movie", SourceRef: "catalog-a"}
	source := Source{Open: func(context.Context, int64, int64) (io.ReadCloser, error) {
		t.Fatal("observation performed source I/O")
		return nil, ErrSource
	}}
	id, err := m.Create(identity, source, Plan{AudioCodec: "copy"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.jobs[id].cancel()
	identity.SourceRef = "catalog-b"
	if got := m.SourceRef("secret-owner", id); got != "catalog-a" {
		t.Fatalf("captured source changed: %q", got)
	}
	if m.SourceRef("other", id) != "" || m.SourceRef("secret-owner", "missing") != "" {
		t.Fatal("source bypassed ownership or missing job")
	}
	m.publishObservation()
	first := m.Snapshot()
	if len(first.Jobs) != 1 || first.Jobs[0].SourceRef != "catalog-a" || first.Jobs[0].ItemID != "movie" {
		t.Fatalf("snapshot lost source: %+v", first)
	}
	data, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-owner") {
		t.Fatal("snapshot exposed owner")
	}
	legacy, err := m.Create(Identity{Owner: "owner", ItemID: "legacy"}, source, Plan{AudioCodec: "copy"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.jobs[legacy].cancel()
	m.publishObservation()
	if len(first.Jobs) != 1 || first.Jobs[0].SourceRef != "catalog-a" {
		t.Fatal("published snapshot mutated")
	}
	if m.SourceRef("owner", legacy) != "" {
		t.Fatal("legacy job gained a source")
	}
}
