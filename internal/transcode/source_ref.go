package transcode

// SourceRef returns the source captured when this owned playback was created.
// The credential-free value is read before transfer I/O, never per chunk.
func (m *Manager) SourceRef(owner, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.get(owner, id)
	if err != nil {
		return ""
	}
	return j.identity.SourceRef
}
