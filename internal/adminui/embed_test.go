package adminui

import (
	"io/fs"
	"testing"
)

func TestDistContainsIndex(t *testing.T) {
	data, err := fs.ReadFile(Dist, "dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty index.html")
	}
}
