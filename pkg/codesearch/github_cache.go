package codesearch

import (
	"crypto/sha1" //nolint:gosec // git object IDs are SHA-1, this is not a security boundary
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// githubCache keeps downloaded files on disk so that a repeated search does
// not download them again. Files are stored under their git blob SHA, which
// the search API reports for every hit: the name of an entry is a checksum of
// its content, so an entry can never be out of date, only unused.
//
// A nil *githubCache is valid and caches nothing.
type githubCache struct {
	dir string
	// sweep removes what an interrupted cs left behind, once per process
	sweep sync.Once
}

// newGithubCache returns the cache configured by the cache_dir parameter:
// absent means the default location in the user's cache directory, an empty
// string turns the cache off.
func newGithubCache(params BackendParams) (*githubCache, error) {
	if raw, present := params["cache_dir"]; present {
		dir, ok := raw.(string)
		if !ok {
			// this includes `cache_dir:` with no value, which YAML reads as
			// null: it could mean "the default" or "none", so say so rather
			// than pick one
			return nil, fmt.Errorf(`parameter 'cache_dir' must be a path, or "" to turn the cache off, got %v`, raw)
		}
		if dir == "" {
			return nil, nil
		}
		expanded, err := expandHome(dir)
		if err != nil {
			return nil, fmt.Errorf("failed to expand path %q: %w", dir, err)
		}
		return &githubCache{dir: expanded}, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		// no home directory, e.g. in a minimal container; searching still works.
		// This runs when the backend is built, before any search supplies a
		// logger in SearchOptions, so it goes to the standard logger.
		logrus.Debugf("File cache disabled: %v", err)
		return nil, nil
	}
	return &githubCache{dir: filepath.Join(base, "cs", "github")}, nil
}

// gitBlobSHA returns the ID git gives to a blob with this content.
func gitBlobSHA(content string) string {
	h := sha1.New() //nolint:gosec // see the import
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// path returns where the blob with this SHA is stored. The SHA comes from the
// API and becomes part of a file name, so anything but a SHA-1 in lowercase
// hex is rejected.
func (c *githubCache) path(sha string) (string, bool) {
	if c == nil || len(sha) != sha1.Size*2 {
		return "", false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	// fan out over 256 directories, like git does
	return filepath.Join(c.dir, sha[:2], sha[2:]), true
}

// load returns the content of the blob with this SHA, if it is in the cache.
func (c *githubCache) load(log logrus.FieldLogger, sha string) (string, bool) {
	path, ok := c.path(sha)
	if !ok {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := string(data)
	if gitBlobSHA(text) != sha {
		// truncated by a crash, or edited; either way it is not this blob
		log.Debugf("Dropping corrupt cache entry %q", path)
		_ = os.Remove(path)
		return "", false
	}
	return text, true
}

// store saves a downloaded file under the SHA of its content. Failing to do
// so only costs a download next time, so it is not an error.
func (c *githubCache) store(log logrus.FieldLogger, text string) {
	path, ok := c.path(gitBlobSHA(text))
	if !ok {
		return
	}
	if err := c.write(path, text); err != nil {
		log.Debugf("Cannot cache file: %v", err)
	}
}

// githubCacheTmpAge is how old a temporary file must be before it is taken
// for the leftover of an interrupted cs rather than the work of a running one.
const githubCacheTmpAge = time.Hour

// sweepTmp removes temporary files that were never renamed into place,
// because cs was interrupted between writing and renaming them.
func (c *githubCache) sweepTmp(tmpDir string) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > githubCacheTmpAge {
			_ = os.Remove(filepath.Join(tmpDir, e.Name()))
		}
	}
}

func (c *githubCache) write(path, text string) error {
	// source code from private repositories ends up here
	tmpDir := filepath.Join(c.dir, "tmp")
	for _, dir := range []string{filepath.Dir(path), tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	c.sweep.Do(func() { c.sweepTmp(tmpDir) })
	// write to a temporary file and rename it, so that a concurrent cs or a
	// crash never leaves a partial entry under the final name. The temporary
	// files have a directory of their own, in the same file system, so that
	// the leftovers can be found again.
	tmp, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(text)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
