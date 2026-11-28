package codesearch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGitBlobSHA checks the hash against values from `git hash-object`.
func TestGitBlobSHA(t *testing.T) {
	for content, want := range map[string]string{
		"":              "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello world\n": "3b18e512dba79e4c8300dd08aeb37f8e728b8dad",
	} {
		if got := gitBlobSHA(content); got != want {
			t.Errorf("gitBlobSHA(%q) = %s, want %s", content, got, want)
		}
	}
}

// cachedGithubServer serves one hit for a file with this content, reporting
// sha for it, and counts the downloads.
func cachedGithubServer(t *testing.T, content, sha string, downloads *int32) *httptest.Server {
	t.Helper()
	item := strings.Replace(codeResultJSON("src/f.go", "needle", 0, 6), `"deadbeef"`, fmt.Sprintf("%q", sha), 1)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/code"):
			searchPage(w, r, item, 0)
		case strings.Contains(r.URL.Path, "/contents/"):
			atomic.AddInt32(downloads, 1)
			_, _ = fmt.Fprint(w, base64File(content))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func cachedGithubBackend(t *testing.T, url string, params BackendParams) *Github {
	t.Helper()
	p := BackendParams{"org": "testorg", "token": "testtoken", "api_endpoint": url}
	for k, v := range params {
		p[k] = v
	}
	b, err := NewGithub("test", p)
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b
}

func githubSearchOne(t *testing.T, b *Github) Result {
	t.Helper()
	results, err := b.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	return results[0]
}

// TestGithubCacheAvoidsDownload: the second search for the same blob, from a
// new backend as a new cs process would have, downloads nothing and gives the
// same result.
func TestGithubCacheAvoidsDownload(t *testing.T) {
	const content = "first\nneedle here\n"
	var downloads int32
	srv := cachedGithubServer(t, content, gitBlobSHA(content), &downloads)
	defer srv.Close()
	params := BackendParams{"cache_dir": t.TempDir()}

	first := githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	second := githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Errorf("got %d downloads, want 1", got)
	}
	if first.Lineno != 2 || first.Line != "needle here" {
		t.Errorf("got line %d %q, want line 2 %q", first.Lineno, first.Line, "needle here")
	}
	if first.Lineno != second.Lineno || first.Line != second.Line || first.FileURL != second.FileURL {
		t.Errorf("cached result %+v differs from downloaded result %+v", second, first)
	}
}

// TestGithubCacheStaleIndex: when the file changed after GitHub indexed it,
// the download is not the blob the hit names, so it must not be served for
// that SHA.
func TestGithubCacheStaleIndex(t *testing.T) {
	const content = "needle\n"
	var downloads int32
	srv := cachedGithubServer(t, content, gitBlobSHA("an older version\n"), &downloads)
	defer srv.Close()
	params := BackendParams{"cache_dir": t.TempDir()}

	githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	if got := atomic.LoadInt32(&downloads); got != 2 {
		t.Errorf("got %d downloads, want 2", got)
	}
}

// TestGithubCacheDropsCorruptEntry: an entry whose content does not hash to
// its name is not used, and is replaced by a fresh download.
func TestGithubCacheDropsCorruptEntry(t *testing.T) {
	const content = "needle\n"
	sha := gitBlobSHA(content)
	var downloads int32
	srv := cachedGithubServer(t, content, sha, &downloads)
	defer srv.Close()
	dir := t.TempDir()
	params := BackendParams{"cache_dir": dir}

	githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	entry := filepath.Join(dir, sha[:2], sha[2:])
	if err := os.WriteFile(entry, []byte("need"), 0o600); err != nil {
		t.Fatalf("failed to corrupt the entry: %v", err)
	}
	if r := githubSearchOne(t, cachedGithubBackend(t, srv.URL, params)); r.Line != "needle" {
		t.Errorf("got line %q, want %q", r.Line, "needle")
	}
	if got := atomic.LoadInt32(&downloads); got != 2 {
		t.Errorf("got %d downloads, want 2", got)
	}
	if data, err := os.ReadFile(entry); err != nil || string(data) != content {
		t.Errorf("entry holds %q (%v), want the fresh download", data, err)
	}
}

// TestGithubCacheDisabled: an empty cache_dir turns the cache off.
func TestGithubCacheDisabled(t *testing.T) {
	const content = "needle\n"
	var downloads int32
	srv := cachedGithubServer(t, content, gitBlobSHA(content), &downloads)
	defer srv.Close()
	// if the cache were on, it would be written below this directory
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	params := BackendParams{"cache_dir": ""}

	githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	githubSearchOne(t, cachedGithubBackend(t, srv.URL, params))
	if got := atomic.LoadInt32(&downloads); got != 2 {
		t.Errorf("got %d downloads, want 2", got)
	}
}

// TestGithubCacheEntryPermissions: cached source code may be private.
func TestGithubCacheEntryPermissions(t *testing.T) {
	c := &githubCache{dir: filepath.Join(t.TempDir(), "cache")}
	c.store("secret\n")
	sha := gitBlobSHA("secret\n")
	for _, p := range []string{c.dir, filepath.Join(c.dir, sha[:2]), filepath.Join(c.dir, sha[:2], sha[2:])} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %q: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%q has mode %o, want no access for group and others", p, perm)
		}
	}
	entries, err := os.ReadDir(filepath.Join(c.dir, sha[:2]))
	if err != nil || len(entries) != 1 {
		t.Errorf("got %d entries (%v), want 1: no temporary file may stay behind", len(entries), err)
	}
}

// TestGithubCacheRejectsBadSHA: the SHA comes from the API and is used in a
// file name.
func TestGithubCacheRejectsBadSHA(t *testing.T) {
	c := &githubCache{dir: t.TempDir()}
	for _, sha := range []string{"", "deadbeef", "../../../../etc/passwd", strings.Repeat("A", 40), strings.Repeat("a", 39) + "/", strings.Repeat("a", 64)} {
		if p, ok := c.path(sha); ok {
			t.Errorf("path(%q) = %q, want it rejected", sha, p)
		}
	}
	var none *githubCache
	if _, ok := none.load(strings.Repeat("a", 40)); ok {
		t.Error("a nil cache returned an entry")
	}
	none.store("ignored")
}

// TestGithubCacheDirParam covers the parameter itself.
func TestGithubCacheDirParam(t *testing.T) {
	if _, err := newGithubCache(BackendParams{"cache_dir": 42}); err == nil {
		t.Error("got nil error for a numeric cache_dir, want a failure")
	}
	t.Setenv("XDG_CACHE_HOME", "/xdg")
	c, err := newGithubCache(BackendParams{})
	if err != nil || c == nil {
		t.Fatalf("got %v, %v, want the default cache", c, err)
	}
	if !strings.HasSuffix(filepath.ToSlash(c.dir), "cs/github") {
		t.Errorf("got default dir %q, want it to end in cs/github", c.dir)
	}
}
