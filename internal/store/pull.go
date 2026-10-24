package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// pullProgress reports a pull's layers (see Store.Progress), one line
// at a time: no animation, so it reads the same in a terminal, a log or
// a CI job.
type pullProgress struct {
	w  io.Writer
	mu sync.Mutex
}

// fetchLayers downloads img's missing layers into the store, a few at a
// time, reporting each one. Layers from a registry are verified against
// their digest as they are read.
func (s *Store) fetchLayers(ctx context.Context, img v1.Image, progress io.Writer) error {
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	p := &pullProgress{w: progress}
	if p.w == nil {
		p.w = io.Discard
	}
	type job struct {
		l    v1.Layer
		d    v1.Hash
		size int64
	}
	var jobs []job
	var total int64
	for _, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return err
		}
		size, err := l.Size()
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(s.dir, "blobs", d.Algorithm, d.Hex)); err == nil {
			p.line("  %s  %8s  already present", short(d), humanSize(size))
			continue
		}
		jobs = append(jobs, job{l, d, size})
		total += size
	}
	if len(jobs) == 0 {
		return nil
	}

	p.line("  downloading %d layer(s), %s", len(jobs), humanSize(total))

	sem := make(chan struct{}, 4)
	errs := make(chan error, len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
			defer func() { <-sem }()
			start := time.Now()
			rc, err := j.l.Compressed()
			if err != nil {
				errs <- fmt.Errorf("layer %s: %w", short(j.d), err)
				return
			}
			err = s.lp.WriteBlob(j.d, rc)
			rc.Close()
			if err != nil {
				errs <- fmt.Errorf("layer %s: %w", short(j.d), err)
				return
			}
			p.line("  %s  %8s  pulled in %s", short(j.d), humanSize(j.size), time.Since(start).Round(100*time.Millisecond))
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs // the first error, or nil
}

func (p *pullProgress) line(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, format+"\n", a...)
}

// short is the abbreviated form of a digest, as image IDs are shown.
func short(d v1.Hash) string {
	if len(d.Hex) < 12 {
		return d.String()
	}
	return d.Hex[:12]
}

func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.3g %cB", v, "kMGT"[exp-1])
}
