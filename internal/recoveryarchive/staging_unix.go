//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
)

// This profile requires an explicit caller-owned 0700 directory. Ancestors and
// the OS account are trusted. Files are unlinked immediately after exclusive
// creation; crash cleanup leaves at most an empty private staging directory.
type staging struct {
	parent   *os.Root
	root     *os.Root
	name     string
	files    []*os.File
	closed   bool
	closeErr error
}

func ownedPrivate(info os.FileInfo, directory bool) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600 && info.Mode().IsRegular()
}

func newStaging(parent string) (*staging, error) {
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent {
		return nil, ErrOptions
	}
	before, err := os.Lstat(parent)
	if err != nil || !ownedPrivate(before, true) {
		return nil, ErrStaging
	}
	p, err := os.OpenRoot(parent)
	if err != nil {
		return nil, ErrStaging
	}
	after, err := p.Stat(".")
	if err != nil || !ownedPrivate(after, true) || !os.SameFile(before, after) {
		_ = p.Close()
		return nil, ErrStaging
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		_ = p.Close()
		return nil, ErrStaging
	}
	name := "recovery-" + hex.EncodeToString(token[:])
	if p.Mkdir(name, 0700) != nil {
		_ = p.Close()
		return nil, ErrStaging
	}
	r, err := p.OpenRoot(name)
	if err != nil {
		_ = p.Remove(name)
		_ = p.Close()
		return nil, ErrStaging
	}
	return &staging{parent: p, root: r, name: name}, nil
}

func (s *staging) anonymousFile(name string) (*os.File, error) {
	if s.closed || (name != "ciphertext" && name != "plaintext") {
		return nil, ErrStaging
	}
	f, err := s.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrStaging
	}
	info, err := f.Stat()
	if err != nil || !ownedPrivate(info, false) || s.root.Remove(name) != nil {
		// A failed validation must not leave a named plaintext file behind.
		// Only the exact file created by this call is eligible for removal.
		if current, e := s.root.Lstat(name); e == nil && info != nil && os.SameFile(info, current) {
			_ = s.root.Remove(name)
		}
		_ = f.Close()
		return nil, ErrStaging
	}
	s.files = append(s.files, f)
	return f, nil
}

func (s *staging) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	failed := false
	for _, file := range s.files {
		if file.Close() != nil {
			failed = true
		}
	}
	if s.root.Close() != nil {
		failed = true
	}
	// Remove only our empty directory, never recursive deletion or adoption.
	if s.parent.Remove(s.name) != nil {
		failed = true
	}
	if s.parent.Close() != nil {
		failed = true
	}
	if failed {
		s.closeErr = ErrStaging
	}
	return s.closeErr
}
