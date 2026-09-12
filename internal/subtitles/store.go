package subtitles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const maxOutput = 8 << 20
const maxArtifacts = 4096
const artifactLifetime = 7 * 24 * time.Hour

type artifact struct {
	key, format string
	size        int64
	pins        int
	last        time.Time
}

// store is accessed with Manager.mu held. Reservations happen before writing,
// and the only temporary file contains the same reserved bytes as its result.
type store struct {
	root         *os.Root
	lock         *os.File
	path         string
	temporary    bool
	budget, used int64
	files        map[string]*artifact
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func openStore(parent string, budget int64) (*store, error) {
	path := ""
	temporary := parent == ""
	var err error
	if temporary {
		path, err = os.MkdirTemp("", "eag-subtitles-")
	} else {
		path = filepath.Join(parent, "eag-subtitle-artifacts-v1")
		err = os.MkdirAll(path, 0700)
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("subtitle cache directory is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s := &store{root: root, path: path, temporary: temporary, budget: budget, files: map[string]*artifact{}}
	fail := func(e error) (*store, error) {
		if s.lock != nil {
			_ = s.lock.Close()
		}
		_ = root.Close()
		return nil, e
	}
	st, err = root.Lstat(".lock")
	if err == nil && !st.Mode().IsRegular() {
		return fail(fmt.Errorf("subtitle cache lock is invalid"))
	}
	s.lock, err = root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	if err = unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("subtitle cache directory is in use: %w", err))
	}
	dir, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	entries, err := dir.ReadDir(maxArtifacts + 4)
	_ = dir.Close()
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return fail(err)
	}
	const marker = "gateway-subtitle-artifacts-v1\n"
	st, err = root.Lstat(".owner")
	if errors.Is(err, os.ErrNotExist) {
		if len(entries) != 1 || entries[0].Name() != ".lock" {
			return fail(fmt.Errorf("subtitle cache directory is not gateway-owned"))
		}
		err = root.WriteFile(".owner", []byte(marker), 0600)
	} else if err == nil {
		if !st.Mode().IsRegular() || st.Size() != int64(len(marker)) {
			return fail(fmt.Errorf("subtitle cache ownership marker is invalid"))
		}
		data, e := root.ReadFile(".owner")
		err = e
		if err == nil && string(data) != marker {
			err = fmt.Errorf("subtitle cache ownership marker is invalid")
		}
	}
	if err != nil {
		return fail(err)
	}
	if len(entries) > maxArtifacts+3 {
		return fail(fmt.Errorf("subtitle artifact entry limit exceeded"))
	}
	for _, e := range entries {
		name := e.Name()
		if name == ".owner" || name == ".lock" {
			continue
		}
		if name == ".partial" {
			if err = root.Remove(name); err != nil {
				return fail(err)
			}
			continue
		}
		key, format, ok := strings.Cut(name, ".")
		decoded, ehex := hex.DecodeString(key)
		if !ok || ehex != nil || len(decoded) != sha256.Size || format != "vtt" && format != "ass" {
			return fail(fmt.Errorf("unrecognized subtitle cache artifact"))
		}
		st, err = root.Lstat(name)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxOutput {
			return fail(fmt.Errorf("invalid subtitle cache artifact"))
		}
		if time.Since(st.ModTime()) > artifactLifetime {
			if err = root.Remove(name); err != nil {
				return fail(err)
			}
			continue
		}
		a := &artifact{key: key, format: format, size: st.Size(), last: st.ModTime()}
		s.files[key] = a
		s.used += a.size
	}
	if !s.space(0) {
		return fail(ErrCapacity)
	}
	return s, nil
}

func (s *store) space(size int64) bool {
	if size < 0 || size > s.budget {
		return false
	}
	for s.used > s.budget-size || len(s.files) >= maxArtifacts {
		var victim *artifact
		for _, a := range s.files {
			if a.pins == 0 && (victim == nil || a.last.Before(victim.last)) {
				victim = a
			}
		}
		if victim == nil {
			return false
		}
		if err := s.root.Remove(victim.key + "." + victim.format); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
		delete(s.files, victim.key)
		s.used -= victim.size
	}
	return true
}

func (s *store) put(key, format string, data []byte) (*artifact, error) {
	if a := s.files[key]; a != nil {
		a.last = time.Now()
		return a, nil
	}
	if len(data) == 0 || len(data) > maxOutput || !s.space(int64(len(data))) {
		return nil, ErrCapacity
	}
	f, err := s.root.OpenFile(".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	n, err := f.Write(data)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.root.Rename(".partial", key+"."+format)
	}
	if err != nil {
		_ = s.root.Remove(".partial")
		return nil, err
	}
	a := &artifact{key: key, format: format, size: int64(len(data)), last: time.Now()}
	s.files[key] = a
	s.used += a.size
	return a, nil
}

func (s *store) read(a *artifact) (*os.File, error) {
	f, err := s.root.Open(a.key + "." + a.format)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != a.size {
		_ = f.Close()
		return nil, ErrUnavailable
	}
	a.last = time.Now()
	return f, nil
}

func (s *store) discard(a *artifact) bool {
	if current := s.files[a.key]; current != a {
		return current == nil
	}
	if a.pins != 0 {
		return false
	}
	if err := s.root.Remove(a.key + "." + a.format); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	delete(s.files, a.key)
	s.used -= a.size
	return true
}

func (s *store) close() error {
	err := s.root.Close()
	if s.temporary {
		err = errors.Join(err, os.RemoveAll(s.path))
	}
	return errors.Join(err, s.lock.Close())
}
