package gateway

import "testing"

func TestMediaFingerprintMatchesPreservesPartialMetadataContract(t *testing.T) {
	cases := []struct {
		name, stored, itemType, title, seriesID string
		want                                    bool
	}{
		{"full match", "type=Episode|name=Episode One|seriesid=series-a", "Episode", "Episode One", "series-a", true},
		{"reused ID different name", "type=Episode|name=Episode One|seriesid=series-a", "Episode", "Another Video", "series-a", false},
		{"reused ID different type", "type=Episode|name=Episode One", "Movie", "Episode One", "", false},
		{"reused ID different series", "type=Episode|name=Episode One|seriesid=series-a", "Episode", "Episode One", "series-b", false},
		{"partial old report", "name=Episode One", "Episode", "Episode One", "series-a", true},
		{"partial current metadata", "type=Episode|name=Episode One|seriesid=series-a", "Episode", "", "", true},
		{"unknown stored fingerprint", "", "Episode", "Episode One", "series-a", true},
		{"unknown current metadata", "type=Episode|name=Episode One|seriesid=series-a", "", "", "", true},
		{"conflicting known field despite unknown name", "type=Episode|name=Episode One", "Movie", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MediaFingerprintMatches(tc.stored, tc.itemType, tc.title, tc.seriesID); got != tc.want {
				t.Fatalf("matches=%v want=%v", got, tc.want)
			}
		})
	}
}
