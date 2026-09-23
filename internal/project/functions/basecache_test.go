/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package functions

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// testLayer returns a gzipped layer, like the ones a registry serves. The
// filesystem cache checks an entry's digest when it reads it back, and a blob
// that is not compressed would be recompressed on the way, so it would never
// match.
func testLayer() v1.Layer {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("hello from a layer"))
	_ = zw.Close()
	return static.NewLayer(buf.Bytes(), types.DockerLayer)
}

func readAll(t *testing.T, l v1.Layer) string {
	t.Helper()
	rc, err := l.Compressed()
	if err != nil {
		t.Fatalf("Compressed(): unexpected error: %v", err)
	}
	defer func() { _ = rc.Close() }()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("reading layer: unexpected error: %v", err)
	}
	bs, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading layer: unexpected error: %v", err)
	}
	return string(bs)
}

// erroringCache fails every operation the way a corrupt or unreadable cache
// directory would, rather than reporting a miss.
type erroringCache struct{}

func (erroringCache) Get(v1.Hash) (v1.Layer, error)  { return nil, errors.New("boom") }
func (erroringCache) Put(v1.Layer) (v1.Layer, error) { return nil, errors.New("boom") }
func (erroringCache) Delete(v1.Hash) error           { return errors.New("boom") }

func TestTolerantCacheGetReportsFailuresAsMisses(t *testing.T) {
	// A read failure that is not a genuine miss must still look like a miss,
	// so the caller falls through to the registry instead of erroring out.
	c := tolerantCache{erroringCache{}}

	_, err := c.Get(v1.Hash{Algorithm: "sha256", Hex: "cafe"})
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get(): want cache.ErrNotFound, got %v", err)
	}
}

func TestTolerantCachePutFailureReturnsUsableLayer(t *testing.T) {
	// Put failing outright must hand back a layer that still reads.
	c := tolerantCache{erroringCache{}}

	l, err := c.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	if diff := cmp.Diff("hello from a layer", readAll(t, l)); diff != "" {
		t.Errorf("layer contents (-want +got):\n%s", diff)
	}
}

func TestTolerantCacheUnwritableDirStillReads(t *testing.T) {
	// The regression this guards: an unwritable cache directory used to fail
	// the build, because the filesystem cache creates its backing file lazily
	// inside Compressed().
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a read-only directory")
	}

	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("creating read-only cache dir: %v", err)
	}

	plain := newFilesystemCache(dir)
	l, err := plain.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	if _, err := l.Compressed(); err == nil {
		t.Skip("cache directory turned out to be writable; nothing to assert")
	}

	// Same directory, wrapped: the layer must read regardless.
	tolerant := tolerantCache{newFilesystemCache(dir)}
	wrapped, err := tolerant.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	if diff := cmp.Diff("hello from a layer", readAll(t, wrapped)); diff != "" {
		t.Errorf("layer contents (-want +got):\n%s", diff)
	}
}

func TestTolerantCachePassesThroughOnSuccess(t *testing.T) {
	// A working cache must behave exactly as it would unwrapped: a miss is
	// still a miss, and a stored layer still reads back.
	c := tolerantCache{newFilesystemCache(t.TempDir())}

	if _, err := c.Get(v1.Hash{Algorithm: "sha256", Hex: "cafe"}); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get() on empty cache: want cache.ErrNotFound, got %v", err)
	}

	l, err := c.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	if diff := cmp.Diff("hello from a layer", readAll(t, l)); diff != "" {
		t.Errorf("layer contents (-want +got):\n%s", diff)
	}

	// Reading the layer populates the cache, so the digest is now a hit.
	d, err := l.Digest()
	if err != nil {
		t.Fatalf("Digest(): unexpected error: %v", err)
	}
	if _, err := c.Get(d); err != nil {
		t.Errorf("Get() after populating cache: unexpected error: %v", err)
	}
}

func TestFilesystemCacheUnreadLayerLeavesNoEntry(t *testing.T) {
	// Opening a layer and closing it without reading it must neither create
	// an entry nor damage an existing one. go-containerregistry's filesystem
	// cache truncated the entry to zero bytes here.
	dir := t.TempDir()
	c := newFilesystemCache(dir)
	l, err := c.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	d, err := l.Digest()
	if err != nil {
		t.Fatalf("Digest(): unexpected error: %v", err)
	}

	rc, err := l.Compressed()
	if err != nil {
		t.Fatalf("Compressed(): unexpected error: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close(): unexpected error: %v", err)
	}
	if _, err := c.Get(d); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get() after unread layer: want cache.ErrNotFound, got %v", err)
	}

	// Populate the entry, then open and abandon the layer again.
	readAll(t, l)
	rc, err = l.Compressed()
	if err != nil {
		t.Fatalf("Compressed(): unexpected error: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close(): unexpected error: %v", err)
	}
	got, err := c.Get(d)
	if err != nil {
		t.Fatalf("Get() after populating cache: unexpected error: %v", err)
	}
	if diff := cmp.Diff("hello from a layer", readAll(t, got)); diff != "" {
		t.Errorf("cached layer contents (-want +got):\n%s", diff)
	}

	// Nothing should be left behind but the entry itself.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(): unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cache directory: want 1 entry, got %d", len(entries))
	}
}

func TestFilesystemCachePartialReadLeavesNoEntry(t *testing.T) {
	c := newFilesystemCache(t.TempDir())
	l, err := c.Put(testLayer())
	if err != nil {
		t.Fatalf("Put(): unexpected error: %v", err)
	}
	d, err := l.Digest()
	if err != nil {
		t.Fatalf("Digest(): unexpected error: %v", err)
	}

	rc, err := l.Compressed()
	if err != nil {
		t.Fatalf("Compressed(): unexpected error: %v", err)
	}
	if _, err := rc.Read(make([]byte, 4)); err != nil {
		t.Fatalf("Read(): unexpected error: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close(): unexpected error: %v", err)
	}

	if _, err := c.Get(d); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get() after partial read: want cache.ErrNotFound, got %v", err)
	}
}

func TestFilesystemCacheGetDropsCorruptEntry(t *testing.T) {
	// Entries written before this cache existed may be truncated. Get must
	// report them as misses and remove them so they are cached afresh.
	dir := t.TempDir()
	c := newFilesystemCache(dir)
	d, err := testLayer().Digest()
	if err != nil {
		t.Fatalf("Digest(): unexpected error: %v", err)
	}
	p := fsCache{dir: dir}.path(d)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatalf("writing truncated entry: %v", err)
	}

	if _, err := c.Get(d); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get() on truncated entry: want cache.ErrNotFound, got %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("truncated entry: want it removed, got Stat() error %v", err)
	}
}

func TestFilesystemCacheSurvivesSideloadOfSharedLayers(t *testing.T) {
	// The regression this guards: baseImageForArch resolves each
	// architecture's layers through its own cache.Image while the cache is
	// cold, so both architectures hold a caching wrapper for any layer they
	// share. Writing both images to an OCI layout (as the local dev control
	// plane's sideload does) reads the shared layer through the first wrapper
	// and opens then abandons it through the second, because the layout
	// already holds the blob. That used to leave a zero-byte entry that the
	// next build baked into its images as an empty layer.
	shared := testLayer()
	d, err := shared.Digest()
	if err != nil {
		t.Fatalf("Digest(): unexpected error: %v", err)
	}
	dir := t.TempDir()

	images := make([]v1.Image, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		src, err := mutate.AppendLayers(empty.Image, shared)
		if err != nil {
			t.Fatalf("AppendLayers(): unexpected error: %v", err)
		}
		src = cache.Image(src, tolerantCache{newFilesystemCache(dir)})
		l, err := src.LayerByDigest(d)
		if err != nil {
			t.Fatalf("LayerByDigest(): unexpected error: %v", err)
		}
		img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{Architecture: arch, OS: "linux"})
		if err != nil {
			t.Fatalf("ConfigFile(): unexpected error: %v", err)
		}
		img, err = mutate.AppendLayers(img, l)
		if err != nil {
			t.Fatalf("AppendLayers(): unexpected error: %v", err)
		}
		images = append(images, img)
	}

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: images[0]},
		mutate.IndexAddendum{Add: images[1]},
	)
	lp, err := layout.Write(t.TempDir(), empty.Index)
	if err != nil {
		t.Fatalf("layout.Write(): unexpected error: %v", err)
	}
	if err := lp.AppendIndex(idx); err != nil {
		t.Fatalf("AppendIndex(): unexpected error: %v", err)
	}

	got, err := newFilesystemCache(dir).Get(d)
	if err != nil {
		t.Fatalf("Get() after sideload: unexpected error: %v", err)
	}
	if diff := cmp.Diff("hello from a layer", readAll(t, got)); diff != "" {
		t.Errorf("cached layer contents (-want +got):\n%s", diff)
	}
}
