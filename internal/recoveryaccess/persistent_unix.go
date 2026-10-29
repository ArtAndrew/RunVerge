//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The OS account, private parent's protected ancestor namespace, and local
// filesystem semantics are trusted. A writable/swappable ancestor is unsupported.
// ACLs must not grant other users access; network/synced filesystems and hostile
// owner/root rollback are outside this profile. No parent or mode is repaired.
type hostDirectory struct {
	parent, root *os.Root
	name         string
	lock         *os.File
	fault        func(string) error
}

func hostOwned(info os.FileInfo, dir bool) bool {
	if info == nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600 && info.Mode().IsRegular() && st.Nlink == 1
}

func hostRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil || !hostOwned(before, true) {
		return nil, ErrHostState
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrHostState
	}
	after, err := r.Stat(".")
	if err != nil || !hostOwned(after, true) || !os.SameFile(before, after) {
		r.Close()
		return nil, ErrHostState
	}
	return r, nil
}

func hostParent(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, "", ErrHostState
	}
	p, err := hostRoot(filepath.Dir(path))
	return p, filepath.Base(path), err
}

func createHostDirectory(ctx context.Context, path string) (*hostDirectory, error) {
	p, name, err := hostParent(path)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || p.Mkdir(name, 0700) != nil {
		p.Close()
		return nil, ErrHostState
	}
	r, err := p.OpenRoot(name)
	if err != nil {
		p.Close()
		return nil, ErrHostState
	}
	d := &hostDirectory{parent: p, root: r, name: name}
	info, err := r.Stat(".")
	if err != nil || !hostOwned(info, true) {
		d.close()
		return nil, ErrHostState
	}
	f, err := r.OpenFile("access.lock", os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		d.close()
		return nil, ErrHostState
	}
	d.lock = f
	if err = f.Sync(); err != nil {
		d.close()
		return nil, ErrHostState
	}
	if err = d.acquire(ctx, true); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func openHostDirectory(ctx context.Context, path string, exclusive bool) (*hostDirectory, error) {
	p, name, err := hostParent(path)
	if err != nil {
		return nil, err
	}
	before, err := p.Lstat(name)
	if err != nil || !hostOwned(before, true) {
		p.Close()
		return nil, ErrHostState
	}
	r, err := p.OpenRoot(name)
	if err != nil {
		p.Close()
		return nil, ErrHostState
	}
	d := &hostDirectory{parent: p, root: r, name: name}
	after, err := r.Stat(".")
	if err != nil || !hostOwned(after, true) || !os.SameFile(before, after) {
		d.close()
		return nil, ErrHostState
	}
	f, err := d.open("access.lock", os.O_RDWR)
	if err != nil {
		d.close()
		return nil, err
	}
	d.lock = f
	if err = d.acquire(ctx, exclusive); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *hostDirectory) open(name string, flags int) (*os.File, error) {
	before, err := d.root.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, ErrHostState
	}
	if !hostOwned(before, false) {
		return nil, ErrHostState
	}
	f, err := d.root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrHostState
	}
	after, err := f.Stat()
	if err != nil || !hostOwned(after, false) || !os.SameFile(before, after) {
		f.Close()
		return nil, ErrHostState
	}
	return f, nil
}

func (d *hostDirectory) acquire(ctx context.Context, exclusive bool) error {
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
			return ErrHostState
		}
		err := syscall.Flock(int(d.lock.Fd()), kind|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return ErrHostState
		}
		select {
		case <-ctx.Done():
			return ErrHostState
		case <-tick.C:
		}
	}
	locked, err := d.lock.Stat()
	current, e := d.root.Lstat("access.lock")
	child, ce := d.parent.Lstat(d.name)
	root, re := d.root.Stat(".")
	if err != nil || e != nil || ce != nil || re != nil || !hostOwned(locked, false) || locked.Size() != 0 || !hostOwned(current, false) || !os.SameFile(locked, current) || !hostOwned(child, true) || !os.SameFile(child, root) {
		return ErrHostState
	}
	return nil
}

func (d *hostDirectory) close() {
	if d.lock != nil {
		_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
		_ = d.lock.Close()
	}
	if d.root != nil {
		_ = d.root.Close()
	}
	if d.parent != nil {
		_ = d.parent.Close()
	}
}
func (d *hostDirectory) checkpoint(name string) error {
	if d.fault != nil {
		return d.fault(name)
	}
	return nil
}

func (d *hostDirectory) read(name string) ([]byte, error) {
	f, err := d.open(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < 1 || info.Size() > 512<<10 {
		return nil, ErrHostState
	}
	raw, err := io.ReadAll(io.LimitReader(f, (512<<10)+1))
	if err != nil || len(raw) > 512<<10 {
		return nil, ErrHostState
	}
	return raw, nil
}

func (d *hostDirectory) load() (hostState, *hostIntent, error) {
	raw, err := d.read("access.json")
	if err != nil {
		return hostState{}, nil, ErrHostState
	}
	s, err := decodeState(raw)
	if err != nil {
		return hostState{}, nil, err
	}
	raw, err = d.read("pending.json")
	if errors.Is(err, os.ErrNotExist) {
		return s, nil, nil
	}
	if err != nil {
		return hostState{}, nil, ErrHostState
	}
	i, err := decodeIntent(raw)
	if err != nil {
		return hostState{}, nil, err
	}
	nextSHA := stateHash(stateBytes(i.Next))
	currentSHA := stateHash(stateBytes(s))
	if i.Next.Info.Binding != s.Info.Binding || i.Next.Info.Epoch < 2 || i.PreviousSHA256 == nextSHA || (currentSHA != i.PreviousSHA256 && currentSHA != nextSHA) {
		return hostState{}, nil, ErrHostState
	}
	if currentSHA == i.PreviousSHA256 && (s.Info.Epoch == ^uint64(0) || i.Next.Info.Epoch != s.Info.Epoch+1) {
		return hostState{}, nil, ErrHostState
	}
	return s, &i, nil
}

func syncHostRoot(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return ErrHostState
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrHostState
	}
	return nil
}
func (d *hostDirectory) syncParent() error { return syncHostRoot(d.parent) }

func (d *hostDirectory) writeIntent(i hostIntent) error {
	raw, _ := json.Marshal(i)
	f, err := d.root.OpenFile("pending.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrPending
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrPending
	}
	if err = d.checkpoint("intent_file_synced"); err != nil {
		return err
	}
	if err = syncHostRoot(d.root); err != nil {
		return err
	}
	return d.checkpoint("intent_directory_synced")
}

func (d *hostDirectory) writeState(s hostState) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrHostState
	}
	name := "state-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrHostState
	}
	defer d.root.Remove(name) // only this call's exclusively created temporary file
	_, err = f.Write(stateBytes(s))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrHostState
	}
	if err = d.checkpoint("state_file_synced"); err != nil {
		return err
	}
	if err = d.root.Rename(name, "access.json"); err != nil {
		return ErrHostState
	}
	if err = d.checkpoint("state_renamed"); err != nil {
		return err
	}
	if err = syncHostRoot(d.root); err != nil {
		return err
	}
	return d.checkpoint("state_directory_synced")
}

func (d *hostDirectory) finish(i hostIntent) error {
	if _, err := i.Next.policy(); err != nil {
		return err
	}
	// Resolve may follow an uncertain intent-write outcome. Establish the durable
	// fence again before changing active state; never infer durability from bytes.
	f, err := d.open("pending.json", os.O_RDWR)
	if err != nil {
		return ErrPending
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrPending
	}
	if err = syncHostRoot(d.root); err != nil {
		return err
	}
	if err = d.checkpoint("resolution_intent_synced"); err != nil {
		return err
	}
	if err := d.writeState(i.Next); err != nil {
		return err
	}
	if err := d.root.Remove("pending.json"); err != nil {
		return ErrPending
	}
	if err := d.checkpoint("intent_removed"); err != nil {
		return err
	}
	if err := syncHostRoot(d.root); err != nil {
		return err
	}
	return d.checkpoint("intent_removal_synced")
}

// SaveHostToken writes only a new private file under a trusted private parent.
// It never prints or overwrites a token; errors leave any created private file
// for explicit inspection rather than retrying against an ambiguous outcome.
func SaveHostToken(path, token string) error {
	if len(token) == 0 || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return ErrHostState
	}
	p, name, err := hostParent(path)
	if err != nil {
		return err
	}
	defer p.Close()
	f, err := p.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrHostState
	}
	_, err = io.WriteString(f, token)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrHostState
	}
	return syncHostRoot(p)
}

func LoadHostToken(path string) (string, error) {
	p, name, err := hostParent(path)
	if err != nil {
		return "", err
	}
	defer p.Close()
	d := hostDirectory{root: p}
	f, err := d.open(name, os.O_RDONLY)
	if err != nil {
		return "", ErrHostState
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) == 0 || len(raw) > 4096 || strings.ContainsAny(string(raw), "\r\n") {
		return "", ErrHostState
	}
	return string(raw), nil
}
