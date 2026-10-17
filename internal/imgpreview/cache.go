package imgpreview

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cache is the content-addressed on-disk preview store:
//
//	<dir>/<aa>/<bb>/<sha256>/<size>.v<ver>.jpg
//
// Files are immutable once written (atomic temp+rename), so reads need no lock
// against eviction: a reader opens its own handle and either gets valid bytes or a
// miss it re-renders. An in-memory index bounds total size (LRU); a per-digest
// zero-byte `decode.v<ver>.fail` marker records a deterministic decode failure so a
// known-bad frame is not re-decoded across sizes.
type cache struct {
	dir      string
	maxBytes int64
	logger   *slog.Logger

	mu    sync.Mutex
	index map[string]*cacheEntry
	total int64
}

type cacheEntry struct {
	bytes int64
	last  int64 // access order (unix nanos)
}

func newCache(dir string, maxBytes int64, logger *slog.Logger) *cache {
	c := &cache{dir: dir, maxBytes: maxBytes, logger: logger, index: map[string]*cacheEntry{}}
	c.startupScan()
	return c
}

// stats returns the current entry count and total bytes under the lock. Safe for the
// metrics state collector: c.mu guards only in-memory maps, and every file I/O
// (removeDir/os.RemoveAll) is deliberately performed outside the lock (hazard P2).
//
// Hazard P3: startupScan mutates index/total WITHOUT c.mu — safe only because
// newCache runs it before the pointer escapes. If a runtime rescan is ever added, it
// must take c.mu or this read becomes a race.
func (c *cache) stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.index), c.total
}

func (c *cache) digestDir(digest string) string {
	return filepath.Join(c.dir, digest[0:2], digest[2:4], digest)
}

func (c *cache) namedFile(size Size) string {
	return fmt.Sprintf("%s.%s.jpg", size, ParamsVersion)
}

func (c *cache) failMarker(digest string) string {
	return filepath.Join(c.digestDir(digest), "decode."+ParamsVersion+".fail")
}

// cachedPath returns (path, true) when the named size is already on disk.
func (c *cache) cachedPath(digest string, size Size) (string, bool) {
	path := filepath.Join(c.digestDir(digest), c.namedFile(size))
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		c.touch(digest)
		return path, true
	}
	return "", false
}

// hasFailMarker reports whether this frame previously failed a deterministic decode.
func (c *cache) hasFailMarker(digest string) bool {
	_, err := os.Stat(c.failMarker(digest))
	return err == nil
}

// writeFailMarker records a deterministic decode failure (best-effort).
func (c *cache) writeFailMarker(digest string) {
	dir := c.digestDir(digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if f, err := os.Create(c.failMarker(digest)); err == nil {
		f.Close()
	}
}

// put writes a rendered named size and returns its path, accounting the bytes and
// evicting LRU digests if the cap is exceeded.
func (c *cache) put(digest string, size Size, data []byte) (string, error) {
	dir := c.digestDir(digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("preview: cache dir: %w", err)
	}
	path := filepath.Join(dir, c.namedFile(size))
	if err := writeFileAtomic(path, data); err != nil {
		return "", err
	}
	victims := c.commit(digest, int64(len(data)))
	for _, v := range victims {
		c.removeDir(v)
	}
	return path, nil
}

// commit accounts n bytes for digest and returns the digest dirs to remove to bring
// total under the cap (never the just-written digest, so a single oversized entry is
// always admitted).
func (c *cache) commit(digest string, n int64) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.index[digest]
	if e == nil {
		e = &cacheEntry{}
		c.index[digest] = e
	}
	e.bytes += n
	e.last = time.Now().UnixNano()
	c.total += n
	return c.pickVictimsLocked(digest)
}

func (c *cache) pickVictimsLocked(keep string) []string {
	var victims []string
	for c.total > c.maxBytes {
		var pick string
		var min int64 = 1<<63 - 1
		for dg, e := range c.index {
			if dg == keep {
				continue
			}
			if e.last < min {
				min, pick = e.last, dg
			}
		}
		if pick == "" {
			break
		}
		c.total -= c.index[pick].bytes
		delete(c.index, pick)
		victims = append(victims, pick)
	}
	return victims
}

func (c *cache) touch(digest string) {
	c.mu.Lock()
	if e := c.index[digest]; e != nil {
		e.last = time.Now().UnixNano()
	}
	c.mu.Unlock()
}

// evictDigest removes a frame's whole cache directory (lifecycle eviction). Best
// effort and non-blocking: a failed removal (a reader holding the file on Windows)
// is reclaimed by a later scan.
func (c *cache) evictDigest(digest string) {
	if len(digest) < 4 {
		return
	}
	c.mu.Lock()
	if e := c.index[digest]; e != nil {
		c.total -= e.bytes
		delete(c.index, digest)
	}
	c.mu.Unlock()
	c.removeDir(digest)
}

func (c *cache) removeDir(digest string) {
	if err := os.RemoveAll(c.digestDir(digest)); err != nil {
		c.logger.Warn("preview: evict digest dir", "digest", digest, "err", err)
	}
}

// startupScan rebuilds the LRU index from disk (bytes + mtime per digest) and deletes
// orphan *.tmp files left by a crash mid-write.
func (c *cache) startupScan() {
	_ = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(path)
			return nil
		}
		if !strings.HasSuffix(name, ".jpg") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		digest := filepath.Base(filepath.Dir(path))
		if len(digest) < 4 {
			return nil
		}
		e := c.index[digest]
		if e == nil {
			e = &cacheEntry{}
			c.index[digest] = e
		}
		e.bytes += fi.Size()
		if m := fi.ModTime().UnixNano(); m > e.last {
			e.last = m
		}
		c.total += fi.Size()
		return nil
	})
	// Enforce the cap after a cold start (e.g. a smaller configured cap than last run).
	victims := func() []string {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.pickVictimsLocked("")
	}()
	for _, v := range victims {
		c.removeDir(v)
	}
}

// writeFileAtomic writes data to a temp file in the same dir and renames it into
// place, so a reader never sees a half-written file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".render-*.tmp")
	if err != nil {
		return fmt.Errorf("preview: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("preview: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("preview: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("preview: commit file: %w", err)
	}
	return nil
}
