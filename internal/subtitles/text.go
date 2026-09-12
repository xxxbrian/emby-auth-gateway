package subtitles

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

var timingLine = regexp.MustCompile(`^(?:[0-9]{2,}:)?[0-5][0-9]:[0-5][0-9][.,][0-9]{3}\s+-->\s+(?:[0-9]{2,}:)?[0-5][0-9]:[0-5][0-9][.,][0-9]{3}(?:\s.*)?$`)

var errEmptyDocument = errors.New("empty subtitle document")

// normalizeText accepts only recognizable, nonempty text-subtitle documents.
// An upstream 200 response containing an HTML error or just WEBVTT is not ready.
func normalizeText(data []byte, format string) ([]byte, string, int, error) {
	if len(data) == 0 {
		return nil, "", 0, errEmptyDocument
	}
	if len(data) > maxOutput || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, "", 0, ErrUnavailable
	}
	s := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	if s == "" {
		return nil, "", 0, errEmptyDocument
	}
	format = strings.ToLower(strings.TrimPrefix(format, "."))
	if format == "subrip" {
		format = "srt"
	}
	if format == "ssa" {
		format = "ass"
	}
	if format == "webvtt" {
		format = "vtt"
	}
	count := 0
	switch format {
	case "vtt", "srt":
		if format == "vtt" && s == "WEBVTT" {
			return nil, "", 0, errEmptyDocument
		}
		if format == "vtt" && !strings.HasPrefix(s, "WEBVTT\n") && !strings.HasPrefix(s, "WEBVTT ") {
			return nil, "", 0, ErrUnavailable
		}
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			if timingLine.MatchString(line) && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
				count++
				if format == "srt" {
					lines[i] = strings.ReplaceAll(line, ",", ".")
				}
			}
		}
		if count == 0 {
			if format == "vtt" {
				return nil, "", 0, errEmptyDocument
			}
			return nil, "", 0, ErrUnavailable
		}
		s = strings.Join(lines, "\n") + "\n"
		if format == "srt" {
			s = "WEBVTT\n\n" + s
		}
		format = "vtt"
	case "ass":
		if !strings.HasPrefix(strings.ToLower(s), "[script info]") || !strings.Contains(strings.ToLower(s), "[events]") {
			return nil, "", 0, ErrUnavailable
		}
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(strings.ToLower(line), "dialogue:") {
				parts := strings.SplitN(line, ",", 10)
				if len(parts) == 10 && strings.TrimSpace(parts[9]) != "" {
					count++
				}
			}
		}
		if count == 0 {
			return nil, "", 0, errEmptyDocument
		}
		s += "\n"
	default:
		return nil, "", 0, ErrUnavailable
	}
	if len(s) > maxOutput {
		return nil, "", 0, ErrCapacity
	}
	return []byte(s), format, count, nil
}

func textCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "srt", "subrip", "vtt", "webvtt", "ass", "ssa":
		return true
	}
	return false
}

func safeLabel(s string) string {
	if strings.Contains(s, "://") {
		return "[media source]"
	}
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > 160 {
		r = r[:160]
	}
	return string(r)
}
