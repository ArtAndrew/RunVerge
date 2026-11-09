// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"context"
	"io"
	"os"
	"sync"
)

// DerivedPayload is read-only, private temporary data, not an authenticated
// archive or import authority. Its owner must check Close before claiming cleanup.
type DerivedPayload struct {
	mu       sync.Mutex
	file     *os.File
	stage    *staging
	closed   bool
	closeErr error
}

// StageDerived writes bounded derived data to an immediately unlinked private
// file. The callback must authenticate its source and validate its output; this
// function supplies only storage isolation and a sealed read-only result.
// A retained callback writer cannot modify the result after the callback returns.
func StageDerived(ctx context.Context, parent string, maxBytes int64, write func(io.Writer) error) (_ *DerivedPayload, resultErr error) {
	if ctx == nil || write == nil || maxBytes < 1 || maxBytes > 384<<20 {
		return nil, ErrOptions
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stage, err := newStaging(parent)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep && stage.Close() != nil {
			resultErr = ErrStaging
		}
	}()
	file, err := stage.anonymousFile("plaintext")
	if err != nil {
		return nil, err
	}
	writer := &derivedWriter{ctx: ctx, file: file, remaining: maxBytes, active: true}
	defer func() { writer.mu.Lock(); writer.active = false; writer.mu.Unlock() }()
	err = write(writer)
	writer.mu.Lock()
	writer.active = false
	if err == nil {
		err = writer.err
	}
	if err == nil {
		err = ctx.Err()
	}
	writer.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if file.Sync() != nil {
		return nil, ErrStaging
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrStaging
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return &DerivedPayload{file: file, stage: stage}, nil
}

type derivedWriter struct {
	mu        sync.Mutex
	ctx       context.Context
	file      *os.File
	remaining int64
	active    bool
	err       error
}

func (w *derivedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return 0, ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = ErrOptions
		return 0, w.err
	}
	n, err := w.file.Write(p)
	w.remaining -= int64(n)
	if err != nil || n != len(p) {
		w.err = ErrIO
		return n, w.err
	}
	return n, nil
}
func (p *DerivedPayload) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrClosed
	}
	n, err := p.file.Read(b)
	if err != nil && err != io.EOF {
		return n, ErrIO
	}
	return n, err
}
func (p *DerivedPayload) ReadAt(b []byte, offset int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrClosed
	}
	n, err := p.file.ReadAt(b, offset)
	if err != nil && err != io.EOF {
		return n, ErrIO
	}
	return n, err
}
func (p *DerivedPayload) Seek(offset int64, whence int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrClosed
	}
	n, err := p.file.Seek(offset, whence)
	if err != nil {
		return 0, ErrIO
	}
	return n, nil
}
func (p *DerivedPayload) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.closeErr = p.stage.Close()
	}
	return p.closeErr
}
