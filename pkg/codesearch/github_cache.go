package codesearch

import (
	"crypto/sha1" //nolint:gosec // git object IDs are SHA-1, this is not a security boundary
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

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
}

// newGithubCache returns the cache configured by the cache_dir parameter:
// absent means the default location in the user's cache directory, an empty
// string turns the cache off.
func newGithubCache(params BackendParams) (*githubCache, error) {
	if raw := params.Get("cache_dir"); raw != nil {
		dir, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("parameter 'cache_dir' must be a path, got %T", raw)
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
		// no home directory, e.g. in a minimal container; searching still works
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
func (c *githubCache) load(sha string) (string, bool) {
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
		logrus.Debugf("Dropping corrupt cache entry %q", path)
		_ = os.Remove(path)
		return "", false
	}
	return text, true
}

// store saves a downloaded file under the SHA of its content. Failing to do
// so only costs a download next time, so it is not an error.
func (c *githubCache) store(text string) {
	path, ok := c.path(gitBlobSHA(text))
	if !ok {
		return
	}
	if err := c.write(path, text); err != nil {
		logrus.Debugf("Cannot cache file: %v", err)
	}
}

func (c *githubCache) write(path, text string) error {
	dir := filepath.Dir(path)
	// source code from private repositories ends up here
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// write to a temporary file and rename it, so that a concurrent cs or a
	// crash never leaves a partial entry under the final name
	tmp, err := os.CreateTemp(dir, "tmp-*")
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
