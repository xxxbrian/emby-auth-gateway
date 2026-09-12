package gateway

// MediaFingerprintMatches applies the existing playback metadata compatibility
// contract to an independent Admin read. Missing metadata is compatible; a
// disagreement in any field present on both sides rejects the association.
// It neither repairs local state nor performs upstream I/O.
func MediaFingerprintMatches(stored, itemType, name, seriesID string) bool {
	return fingerprintsCompatible(stored, itemFingerprint(map[string]any{"Type": itemType, "Name": name, "SeriesId": seriesID}))
}
