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
	"encoding/hex"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// DefaultBaseImageCacheDir returns the default per-user cache directory for
// function runtime base image layers, or "" to disable caching when there is no
// per-user directory to put it in. It sits beside the xpkg cache rather than
// inside it, since the two hold different kinds of artifact and would be pruned
// on different terms.
//
// Falling back to os.TempDir() would put the cache at a predictable path that
// another user on a shared machine could have created first, with permissions
// of their choosing. The layers are public registry content rather than
// anything sensitive, but a cache is not worth a world-readable directory
// someone else controls — and callers already treat "" as "do not cache", so
// declining is cheap.
//
// Nothing prunes this directory. Layers are keyed by content digest, so entries
// are never stale, but they are also never replaced: every base image version a
// user builds against accumulates. Users can delete the directory safely — the
// next build refetches what it needs — but the CLI should grow a retention
// policy or a prune command before this becomes the kind of thing people
// discover by running out of disk.
func DefaultBaseImageCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "crossplane", "base-images")
}

// newFilesystemCache returns a cache of compressed layer blobs in dir, each in a
// file named for its digest.
//
// It stands in for go-containerregistry's cache.NewFilesystemCache, which
// creates an entry's final file when a layer is opened and fills it as the
// layer is read. Any caller that opens a layer without reading it to the end
// leaves a truncated entry behind, and that cache serves it on the next Get
// without checking it. go-containerregistry's own layout writer is such a
// caller: it opens a layer before checking whether the layout already holds
// the blob, then closes it unread. Multi-architecture base images share most
// of their layers, so sideloading them truncated every shared entry to zero
// bytes, and later builds baked those empty layers into function images.
//
// This cache writes an entry to a temporary file and renames it into place
// only once the layer has been read to the end and its content matches its
// digest, so a partial read never produces an entry. Get checks the digest of
// what it finds too, and deletes an entry that does not match, which clears
// out entries written by older versions of the CLI or truncated some other
// way. That check costs nothing extra: tarball.LayerFromFile hashes the file
// on open regardless.
func newFilesystemCache(dir string) cache.Cache {
	return fsCache{dir: dir}
}

type fsCache struct {
	dir string
}

func (c fsCache) path(h v1.Hash) string {
	// ':' is not allowed in Windows file names. This matches the naming
	// go-containerregistry's filesystem cache uses, so existing entries are
	// still found (and checked).
	if runtime.GOOS == "windows" {
		return filepath.Join(c.dir, h.Algorithm+"-"+h.Hex)
	}
	return filepath.Join(c.dir, h.String())
}

// Get returns the cached layer with digest h, or cache.ErrNotFound if there is
// no entry for it or the entry does not hash to h.
func (c fsCache) Get(h v1.Hash) (v1.Layer, error) {
	p := c.path(h)
	l, err := tarball.LayerFromFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, cache.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d, err := l.Digest()
	if err != nil {
		return nil, err
	}
	if d != h {
		// Truncated or otherwise corrupt. Remove it so the layer is fetched and
		// cached afresh.
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, cache.ErrNotFound
	}
	return l, nil
}

// Put returns a layer that adds itself to the cache when its compressed
// content is read to the end.
func (c fsCache) Put(l v1.Layer) (v1.Layer, error) {
	d, err := l.Digest()
	if err != nil {
		return nil, err
	}
	return &fsCacheLayer{Layer: l, cache: c, digest: d}, nil
}

// Delete removes the entry for h.
func (c fsCache) Delete(h v1.Hash) error {
	err := os.Remove(c.path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return cache.ErrNotFound
	}
	return err
}

type fsCacheLayer struct {
	v1.Layer

	cache  fsCache
	digest v1.Hash
}

// Compressed returns the layer's compressed content, copying it into a
// temporary file in the cache directory as it is read.
func (l *fsCacheLayer) Compressed() (io.ReadCloser, error) {
	hasher, err := v1.Hasher(l.digest.Algorithm)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.cache.dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(l.cache.dir, l.digest.Hex+".tmp-*")
	if err != nil {
		return nil, err
	}
	rc, err := l.Layer.Compressed()
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	return &fillingReader{
		rc:     rc,
		tmp:    tmp,
		hasher: hasher,
		want:   l.digest,
		dst:    l.cache.path(l.digest),
	}, nil
}

// fillingReader copies what it reads into tmp and, on Close, moves tmp to dst
// if everything up to EOF was read and it hashes to want. Otherwise it throws
// tmp away.
type fillingReader struct {
	rc     io.ReadCloser
	tmp    *os.File
	hasher hash.Hash
	want   v1.Hash
	dst    string
	eof    bool
}

func (r *fillingReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 && r.tmp != nil {
		if _, werr := r.tmp.Write(p[:n]); werr != nil {
			// A full or failing disk is no reason to fail the read. Stop
			// caching this layer and keep streaming it.
			r.discard()
		} else {
			_, _ = r.hasher.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) {
		r.eof = true
	}
	return n, err
}

func (r *fillingReader) Close() error {
	err := r.rc.Close()
	if r.tmp == nil {
		return err
	}
	got := v1.Hash{Algorithm: r.want.Algorithm, Hex: hex.EncodeToString(r.hasher.Sum(nil))}
	if !r.eof || got != r.want {
		r.discard()
		return err
	}
	name := r.tmp.Name()
	if cerr := r.tmp.Close(); cerr != nil {
		_ = os.Remove(name)
		return err
	}
	if rerr := os.Rename(name, r.dst); rerr != nil {
		_ = os.Remove(name)
	}
	return err
}

func (r *fillingReader) discard() {
	_ = r.tmp.Close()
	_ = os.Remove(r.tmp.Name())
	r.tmp = nil
}

// tolerantCache degrades to the registry instead of failing the build when the
// cache itself cannot be used.
//
// go-containerregistry's cache.Image is not forgiving on its own. A read error
// that is not ErrNotFound propagates out of it, and the filesystem cache
// creates its backing file when a layer is opened, so an unwritable or full
// cache directory surfaces as an error from the layer rather than a cache
// miss. Neither should break a build that could have fetched the layer
// remotely — especially since nothing prunes this cache, which makes a full
// disk a plausible way to reach it. See DefaultBaseImageCacheDir.
//
// A disk that fills up partway through a layer is handled by the filesystem
// cache itself, which stops caching that layer and keeps streaming it. See
// newFilesystemCache.
type tolerantCache struct {
	cache.Cache
}

// Get reports any failure other than a genuine miss as a miss, so that an
// unreadable or corrupt entry sends the caller to the registry.
func (c tolerantCache) Get(h v1.Hash) (v1.Layer, error) {
	l, err := c.Cache.Get(h)
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return nil, cache.ErrNotFound
	}
	return l, err
}

// Put keeps the original layer alongside the caching one so that a layer which
// cannot be written still reads.
func (c tolerantCache) Put(l v1.Layer) (v1.Layer, error) {
	cached, err := c.Cache.Put(l)
	if err != nil {
		// Deliberate: a cache that cannot store the layer is not a reason to
		// fail. Hand back the uncached layer and carry on.
		return l, nil //nolint:nilerr // Cache failures degrade to no caching.
	}
	return tolerantLayer{Layer: cached, uncached: l}, nil
}

// tolerantLayer reads through to an uncached layer when the caching layer
// cannot open its backing file.
type tolerantLayer struct {
	v1.Layer

	uncached v1.Layer
}

func (l tolerantLayer) Compressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return l.uncached.Compressed()
	}
	return rc, nil
}

func (l tolerantLayer) Uncompressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Uncompressed()
	if err != nil {
		return l.uncached.Uncompressed()
	}
	return rc, nil
}
