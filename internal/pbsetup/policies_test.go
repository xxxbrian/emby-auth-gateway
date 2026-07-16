package pbsetup

import (
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
)

func TestInstallDefaultsIsIdempotentAndPreservesMatches(t *testing.T) {
	app := newTestApp(t)
	first := pathpolicy.Defaults()[0]
	records, err := app.FindRecordsByFilter("path_policies", "method = {:method} && path = {:path}", "", 0, 0, map[string]any{"method": first.Method, "path": first.Path})
	if err != nil || len(records) != 1 {
		t.Fatalf("find default: %v %#v", err, records)
	}
	record := records[0]
	record.Set("action", "allow")
	record.Set("enabled", false)
	record.Set("reason", "edited")
	record.Set("priority", 1)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	if created, preserved, err := installDefaults(app); err != nil || created != 0 || preserved != len(pathpolicy.Defaults()) {
		t.Fatalf("rerun = %d/%d/%v", created, preserved, err)
	}
	updated, err := app.FindRecordById("path_policies", record.Id)
	if err != nil || updated.GetString("action") != "allow" || updated.GetBool("enabled") || updated.GetString("reason") != "edited" || updated.GetInt("priority") != 1 {
		t.Fatalf("matching record changed: %v %#v", err, updated)
	}
	if err := app.Delete(record); err != nil {
		t.Fatal(err)
	}
	if created, preserved, err := installDefaults(app); err != nil || created != 1 || preserved != len(pathpolicy.Defaults())-1 {
		t.Fatalf("restore = %d/%d/%v", created, preserved, err)
	}
}

func TestInstallDefaultsMatchesNormalizedIdentity(t *testing.T) {
	app := newTestApp(t)
	first := pathpolicy.Defaults()[0]
	records, _ := app.FindRecordsByFilter("path_policies", "method = {:method} && path = {:path}", "", 0, 0, map[string]any{"method": first.Method, "path": first.Path})
	record := records[0]
	record.Set("method", "post")
	record.Set("path", first.Path+"///")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	if created, preserved, err := installDefaults(app); err != nil || created != 0 || preserved != len(pathpolicy.Defaults()) {
		t.Fatalf("normalized install = %d/%d/%v", created, preserved, err)
	}
}
