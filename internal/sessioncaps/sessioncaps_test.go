package sessioncaps

import (
	"strings"
	"testing"
)

func TestParseEmptyDefaultAndRejects(t *testing.T) {
	t.Parallel()
	empty, err := Parse("")
	if err != nil || empty.RawJSON != "{}" {
		t.Fatalf("empty default = %#v, %v", empty, err)
	}
	if err := Validate(""); err == nil {
		t.Fatal("Validate empty: want error")
	}
	for _, bad := range []string{`null`, `[]`, `{`, `{}{}`, `"x"`, `42`, `true`} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q): want error", bad)
		}
		if err := Validate(bad); err == nil {
			t.Fatalf("Validate(%q): want error", bad)
		}
	}
	huge := `{"Pad":"` + strings.Repeat("x", MaxBytes) + `"}`
	if _, err := Parse(huge); err == nil {
		t.Fatal("oversize: want error")
	}
}

func TestParseKnownFieldBoundsAndTypes(t *testing.T) {
	t.Parallel()
	if _, err := Parse(`{"SupportsSync":"yes"}`); err == nil {
		t.Fatal("wrong bool type: want error")
	}
	if _, err := Parse(`{"PlayableMediaTypes":"Video"}`); err == nil {
		t.Fatal("array wrong type: want error")
	}
	if _, err := Parse(`{"PlayableMediaTypes":[1]}`); err == nil {
		t.Fatal("non-string array item: want error")
	}
	media := make([]string, MaxPlayableMediaTypes+1)
	for i := range media {
		media[i] = "M" + strings.Repeat("a", 1) + string(rune('0'+i%10))
	}
	// unique over-count
	parts := make([]string, MaxPlayableMediaTypes+1)
	for i := range parts {
		parts[i] = "Media" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('A'+i%26))
	}
	// simpler unique names
	for i := range parts {
		parts[i] = "T" + itoa(i)
	}
	body := `{"PlayableMediaTypes":[`
	for i, p := range parts {
		if i > 0 {
			body += ","
		}
		body += `"` + p + `"`
	}
	body += `]}`
	if _, err := Parse(body); err == nil {
		t.Fatal("too many media types: want error")
	}
	long := strings.Repeat("z", MaxPlayableMediaTypeLen+1)
	if _, err := Parse(`{"PlayableMediaTypes":["` + long + `"]}`); err == nil {
		t.Fatal("string too long: want error")
	}
	for _, bad := range []string{`{"DeviceProfile":[]}`, `{"DeviceProfile":"x"}`, `{"DeviceProfile":1}`} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("DeviceProfile %s: want error", bad)
		}
	}
}

func TestParseCanonicalIdempotentAndLargeInts(t *testing.T) {
	t.Parallel()
	const huge = "9007199254740993"
	// Whitespace + key order variants of nested unknown structure.
	variants := []string{
		`{"Custom": { "Nested" : [ 1 , ` + huge + ` ] , "Z": true } , "Huge": ` + huge + ` , "SupportsSync": false}`,
		`{"SupportsSync":false,"Huge":` + huge + `,"Custom":{"Z":true,"Nested":[1,` + huge + `]}}`,
		"{\n  \"Huge\" : " + huge + ",\n  \"Custom\" : { \"Nested\" : [1, " + huge + "], \"Z\" : true },\n  \"SupportsSync\" : false\n}",
	}
	var canonical string
	for i, raw := range variants {
		doc, err := Parse(raw)
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if !strings.Contains(doc.RawJSON, huge) {
			t.Fatalf("variant %d lost large int: %q", i, doc.RawJSON)
		}
		if strings.Count(doc.RawJSON, huge) < 2 {
			t.Fatalf("variant %d expected Huge and Nested int: %q", i, doc.RawJSON)
		}
		if canonical == "" {
			canonical = doc.RawJSON
		} else if doc.RawJSON != canonical {
			t.Fatalf("variant %d not identical\n got %q\nwant %q", i, doc.RawJSON, canonical)
		}
		// Idempotent: re-parse canonical yields the same bytes.
		again, err := Parse(doc.RawJSON)
		if err != nil {
			t.Fatalf("reparse %d: %v", i, err)
		}
		if again.RawJSON != doc.RawJSON {
			t.Fatalf("not idempotent: %q vs %q", again.RawJSON, doc.RawJSON)
		}
	}
	// DeviceProfile nested large int preserved and compacted.
	dp, err := Parse(`{"DeviceProfile":{ "Name" : "x" , "Bitrate" : ` + huge + ` }}`)
	if err != nil {
		t.Fatalf("device profile: %v", err)
	}
	if !strings.Contains(dp.RawJSON, huge) || !strings.Contains(dp.RawJSON, `"DeviceProfile":{`) {
		t.Fatalf("device profile canonical = %q", dp.RawJSON)
	}
	// null DeviceProfile accepted.
	if _, err := Parse(`{"DeviceProfile":null}`); err != nil {
		t.Fatalf("null DeviceProfile: %v", err)
	}
}

func TestValidateAcceptsUnknownFields(t *testing.T) {
	t.Parallel()
	if err := Validate(`{"CustomFlag":true,"Extra":{"A":1}}`); err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
	if err := Validate(`{}`); err != nil {
		t.Fatalf("empty object: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	n := i
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}
