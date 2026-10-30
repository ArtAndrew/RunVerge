//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Private parent, its protected ancestors, ACLs, local filesystem semantics and
// the OS account are trusted. File paths are host inputs, never archive entries.
type hostBundle struct {
	parent, root *os.Root
	name         string
	lock         *os.File
}

func hostPrivateParent(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, "", ErrHostFiles
	}
	parent := filepath.Dir(path)
	before, err := os.Lstat(parent)
	if err != nil || !ownedPrivate(before, true) {
		return nil, "", ErrHostFiles
	}
	r, err := os.OpenRoot(parent)
	if err != nil {
		return nil, "", ErrHostFiles
	}
	after, err := r.Stat(".")
	if err != nil || !ownedPrivate(after, true) || !os.SameFile(before, after) {
		r.Close()
		return nil, "", ErrHostFiles
	}
	return r, filepath.Base(path), nil
}

func hostRegular(info os.FileInfo) bool {
	if info == nil || !ownedPrivate(info, false) {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}

func hostOpen(root *os.Root, name string, flags int) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil || !hostRegular(before) {
		return nil, ErrHostFiles
	}
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrHostFiles
	}
	after, err := f.Stat()
	if err != nil || !hostRegular(after) || !os.SameFile(before, after) {
		f.Close()
		return nil, ErrHostFiles
	}
	return f, nil
}

func hostCreate(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrHostFiles
	}
	info, err := f.Stat()
	if err != nil || !hostRegular(info) {
		f.Close()
		return nil, ErrHostFiles
	}
	return f, nil
}

func hostSync(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return ErrHostFiles
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrHostFiles
	}
	return nil
}

func writeHostPrivateFile(path string, raw []byte) error {
	if len(raw) < 1 || len(raw) > 1<<20 {
		return ErrHostFiles
	}
	p, name, err := hostPrivateParent(path)
	if err != nil {
		return err
	}
	defer p.Close()
	f, err := hostCreate(p, name)
	if err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err == nil && n == len(raw) {
		err = f.Sync()
	} else if err == nil {
		err = io.ErrShortWrite
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrHostFiles
	}
	return hostSync(p)
}

func readHostPrivateFile(path string, max int64) ([]byte, error) {
	p, name, err := hostPrivateParent(path)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	f, err := hostOpen(p, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > max {
		return nil, ErrHostFiles
	}
	return raw, nil
}

func newHostBundle(ctx context.Context, path string) (*hostBundle, error) {
	p, name, err := hostPrivateParent(path)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || p.Mkdir(name, 0700) != nil {
		p.Close()
		return nil, ErrHostFiles
	}
	r, err := p.OpenRoot(name)
	if err != nil {
		p.Close()
		return nil, ErrHostFiles
	}
	b := &hostBundle{parent: p, root: r, name: name}
	info, err := r.Stat(".")
	if err != nil || !ownedPrivate(info, true) {
		b.close()
		return nil, ErrHostFiles
	}
	b.lock, err = hostCreate(r, "backup.lock")
	if err != nil {
		b.close()
		return nil, err
	}
	if err = b.lock.Sync(); err != nil {
		b.close()
		return nil, ErrHostFiles
	}
	if err = b.acquire(ctx, true); err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

func openHostBundle(ctx context.Context, path string) (*hostBundle, error) {
	p, name, err := hostPrivateParent(path)
	if err != nil {
		return nil, err
	}
	before, err := p.Lstat(name)
	if err != nil || !ownedPrivate(before, true) {
		p.Close()
		return nil, ErrHostFiles
	}
	r, err := p.OpenRoot(name)
	if err != nil {
		p.Close()
		return nil, ErrHostFiles
	}
	b := &hostBundle{parent: p, root: r, name: name}
	after, err := r.Stat(".")
	if err != nil || !ownedPrivate(after, true) || !os.SameFile(before, after) {
		b.close()
		return nil, ErrHostFiles
	}
	b.lock, err = hostOpen(r, "backup.lock", os.O_RDWR)
	if err != nil {
		b.close()
		return nil, err
	}
	if err = b.acquire(ctx, false); err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

func (b *hostBundle) acquire(ctx context.Context, exclusive bool) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	kind := syscall.LOCK_SH
	if exclusive {
		kind = syscall.LOCK_EX
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return ErrHostFiles
		}
		err := syscall.Flock(int(b.lock.Fd()), kind|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return ErrHostFiles
		}
		select {
		case <-ctx.Done():
			return ErrHostFiles
		case <-tick.C:
		}
	}
	locked, err := b.lock.Stat()
	named, e := b.root.Lstat("backup.lock")
	root, re := b.root.Stat(".")
	child, ce := b.parent.Lstat(b.name)
	if err != nil || e != nil || re != nil || ce != nil || !hostRegular(locked) || locked.Size() != 0 || !hostRegular(named) || !os.SameFile(locked, named) || !ownedPrivate(child, true) || !os.SameFile(root, child) {
		return ErrHostFiles
	}
	return nil
}

func (b *hostBundle) create(name string) (*os.File, error) { return hostCreate(b.root, name) }
func (b *hostBundle) open(name string) (*os.File, error)   { return hostOpen(b.root, name, os.O_RDONLY) }
func (b *hostBundle) write(name string, raw []byte) error {
	f, err := b.create(name)
	if err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err == nil && n == len(raw) {
		err = f.Sync()
	} else if err == nil {
		err = io.ErrShortWrite
	}
	closed := f.Close()
	if err != nil || closed != nil {
		return ErrHostFiles
	}
	return nil
}
func (b *hostBundle) read(name string, max int64) ([]byte, error) {
	f, err := b.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > max {
		return nil, ErrHostFiles
	}
	return raw, nil
}
func (b *hostBundle) sync() error {
	if err := hostSync(b.root); err != nil {
		return err
	}
	return hostSync(b.parent)
}
func (b *hostBundle) close() {
	if b.lock != nil {
		_ = syscall.Flock(int(b.lock.Fd()), syscall.LOCK_UN)
		_ = b.lock.Close()
	}
	if b.root != nil {
		_ = b.root.Close()
	}
	if b.parent != nil {
		_ = b.parent.Close()
	}
}
