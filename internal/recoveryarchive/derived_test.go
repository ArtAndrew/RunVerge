// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDerivedPrivateSealedLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("private anonymous staging platform")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	var retained io.Writer
	p, err := StageDerived(context.Background(), parent, 3, func(w io.Writer) error {
		retained = w
		_, err := w.Write([]byte("abc"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := filepath.WalkDir(parent, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Fatal("derived plaintext has a name")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatal("retained writer remained active", err)
	}
	raw, err := io.ReadAll(p)
	if err != nil || string(raw) != "abc" {
		t.Fatal("derived bytes changed", err)
	}
	b := make([]byte, 2)
	if n, err := p.ReadAt(b, 1); err != nil || n != 2 || string(b) != "bc" {
		t.Fatal("random access", err)
	}
	if _, err := p.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read(b); !errors.Is(err, ErrClosed) {
		t.Fatal("closed payload readable")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("staging cleanup incomplete", err)
	}
	t.Run("panic_cleanup", func(t *testing.T) {
		var writer io.Writer
		func() {
			defer func() {
				if recover() == nil {
					t.Error("callback panic disappeared")
				}
			}()
			_, _ = StageDerived(context.Background(), parent, 3, func(w io.Writer) error {
				writer = w
				_, _ = w.Write([]byte("a"))
				panic("synthetic callback failure")
			})
		}()
		if writer == nil {
			t.Fatal("callback not reached")
		}
		if _, err := writer.Write([]byte("b")); !errors.Is(err, ErrClosed) {
			t.Fatal("panic left writer active", err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 0 {
			t.Fatal("panic left staging", err)
		}
	})
	for _, mode := range []string{"overflow_swallowed", "callback_error", "canceled_swallowed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, err := StageDerived(ctx, parent, 3, func(w io.Writer) error {
				switch mode {
				case "overflow_swallowed":
					_, _ = w.Write([]byte("abcd"))
					return nil
				case "canceled_swallowed":
					cancel()
					_, _ = w.Write([]byte("a"))
					return nil
				default:
					_, _ = w.Write([]byte("a"))
					return io.ErrUnexpectedEOF
				}
			})
			if err == nil || p != nil {
				t.Fatal("failed derivation returned reader")
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed derivation left staging", err)
			}
		})
	}
}
