package codesearch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v76/github"
	"github.com/sirupsen/logrus"
)

func rateLimitError(reset time.Time) error {
	return &github.RateLimitError{
		Rate:    github.Rate{Reset: github.Timestamp{Time: reset}},
		Message: "API rate limit exceeded",
	}
}

func TestRetrySucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := retry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("got %d calls, want 1", calls)
	}
}

func TestRetryDoesNotRetryOtherErrors(t *testing.T) {
	want := errors.New("boom")
	calls := 0
	err := retry(context.Background(), func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Errorf("got error %v, want %v", err, want)
	}
	if calls != 1 {
		t.Errorf("got %d calls, want 1", calls)
	}
}

// TestRetryGivesUpAfterMaxAttempts is the regression test for the infinite
// loop: the old code drove pagination from the retry counter, so a search that
// stayed rate limited restarted the inner loop forever and never returned.
func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- retry(context.Background(), func() error {
			calls++
			return rateLimitError(time.Now().Add(-time.Hour))
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got nil error, want failure after exhausting attempts")
		}
		var rlErr *github.RateLimitError
		if !errors.As(err, &rlErr) {
			t.Errorf("got error %v, want it to wrap a RateLimitError", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retry did not terminate")
	}
	if calls != maxAttempts {
		t.Errorf("got %d calls, want %d", calls, maxAttempts)
	}
}

func TestRetryRecoversAfterRateLimit(t *testing.T) {
	calls := 0
	err := retry(context.Background(), func() error {
		calls++
		if calls < maxAttempts {
			return rateLimitError(time.Now().Add(-time.Hour))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if calls != maxAttempts {
		t.Errorf("got %d calls, want %d", calls, maxAttempts)
	}
}

func TestRetryStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retry(ctx, func() error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("got %d calls, want 0", calls)
	}
}

func TestRetryStopsWhenContextExpiresDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := retry(ctx, func() error {
		return rateLimitError(time.Now().Add(time.Hour))
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got error %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want it to give up at the deadline", elapsed)
	}
}

// TestRateLimitDelayIsNeverNegative covers the busy loop: once the reset time
// has passed, time.Until returns a negative duration, which time.Sleep treats
// as no wait at all, so the old code hammered the API as fast as it could.
func TestRateLimitDelayIsNeverNegative(t *testing.T) {
	for _, reset := range []time.Time{
		time.Now().Add(-time.Hour),
		time.Now().Add(-time.Second),
		{},
	} {
		delay, retryable := rateLimitDelay(rateLimitError(reset))
		if !retryable {
			t.Errorf("reset %v: got retryable false, want true", reset)
		}
		if delay < 0 {
			t.Errorf("reset %v: got delay %s, want >= 0", reset, delay)
		}
	}
}

func TestRateLimitDelayFuture(t *testing.T) {
	delay, retryable := rateLimitDelay(rateLimitError(time.Now().Add(time.Minute)))
	if !retryable {
		t.Fatal("got retryable false, want true")
	}
	if delay <= 0 || delay > time.Minute {
		t.Errorf("got delay %s, want between 0 and 1m", delay)
	}
}

func TestRateLimitDelaySecondaryLimit(t *testing.T) {
	after := 3 * time.Second
	delay, retryable := rateLimitDelay(&github.AbuseRateLimitError{RetryAfter: &after})
	if !retryable {
		t.Fatal("got retryable false, want true")
	}
	if delay != after {
		t.Errorf("got delay %s, want %s", delay, after)
	}

	negative := -time.Second
	delay, retryable = rateLimitDelay(&github.AbuseRateLimitError{RetryAfter: &negative})
	if !retryable || delay < 0 {
		t.Errorf("got delay %s retryable %v, want >= 0 and true", delay, retryable)
	}

	if _, retryable := rateLimitDelay(errors.New("boom")); retryable {
		t.Error("got retryable true for a plain error, want false")
	}
}

// newGithubBackend points a Github backend at a test server.
func newGithubBackend(t *testing.T, url string) *Github {
	t.Helper()
	b, err := (&Github{}).New("test", BackendParams{
		"org":          "testorg",
		"token":        "testtoken",
		"api_endpoint": url,
	})
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b.(*Github)
}

func searchPage(w http.ResponseWriter, r *http.Request, items string, nextPage int) {
	if nextPage > 0 {
		u := *r.URL
		q := u.Query()
		q.Set("page", strconv.Itoa(nextPage))
		u.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://example.com%s>; rel="next"`, u.RequestURI()))
	}
	fmt.Fprintf(w, `{"total_count":1,"incomplete_results":false,"items":[%s]}`, items)
}

// TestGithubSearchPaginates checks that pagination is driven purely by the
// Link header now that it no longer shares a counter with the retry logic.
func TestGithubSearchPaginates(t *testing.T) {
	var searches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/search/code") {
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&searches, 1)
		if n < 3 {
			searchPage(w, r, "", int(n)+1)
			return
		}
		searchPage(w, r, "", 0)
	}))
	defer srv.Close()

	if _, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := atomic.LoadInt32(&searches), int32(3); got != want {
		t.Errorf("got %d search requests, want %d", got, want)
	}
}

// TestGithubSearchTerminatesWhenRateLimited is the end-to-end version of the
// infinite loop: a server that is always rate limited must produce an error
// rather than spinning.
func TestGithubSearchTerminatesWhenRateLimited(t *testing.T) {
	var searches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searches, 1)
		w.Header().Set("X-RateLimit-Limit", "30")
		w.Header().Set("X-RateLimit-Remaining", "0")
		// already in the past, so the retry must not wait on it
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() {
		_, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got nil error, want a rate limit failure")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("search did not terminate while rate limited")
	}
	if got := atomic.LoadInt32(&searches); got != int32(maxAttempts) {
		t.Errorf("got %d search requests, want %d", got, maxAttempts)
	}
}

func TestGithubSearchHonoursTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		searchPage(w, r, "", 0)
	}))
	defer srv.Close()

	g := newGithubBackend(t, srv.URL)
	start := time.Now()
	_, err := g.Search(context.Background(), "needle", SearchOptions{Timeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("got nil error, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("took %s, want it to stop at the timeout", elapsed)
	}
}

// codeResultJSON builds one search hit with a single text match, so that
// toResult exercises the file content path.
func codeResultJSON(path, fragment string, start, end int) string {
	return fmt.Sprintf(`{
		"name": %q,
		"path": %q,
		"sha": "deadbeef",
		"html_url": "https://example.com/testorg/repo/blob/main/%s",
		"repository": {
			"name": "repo",
			"html_url": "https://example.com/testorg/repo",
			"owner": {"login": "testorg"}
		},
		"text_matches": [{
			"object_url": "https://example.com/obj",
			"object_type": "FileContent",
			"property": "content",
			"fragment": %q,
			"matches": [{"text": "needle", "indices": [%d, %d]}]
		}]
	}`, path, path, path, fragment, start, end)
}

// githubServer serves one page of search results plus a contents endpoint.
func githubServer(t *testing.T, item string, contents func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/code"):
			searchPage(w, r, item, 0)
		case strings.Contains(r.URL.Path, "/contents/"):
			contents(w)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func fileContents(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { fmt.Fprint(w, body) }
}

func base64File(content string) string {
	return fmt.Sprintf(`{"type":"file","encoding":"base64","name":"f.go","path":"src/f.go","content":%q}`,
		base64.StdEncoding.EncodeToString([]byte(content)))
}

func TestGithubSearchDecodesFileContent(t *testing.T) {
	file := "package main\n\nfunc needle() {}\n"
	srv := githubServer(t, codeResultJSON("src/f.go", "func needle() {}", 5, 11),
		fileContents(base64File(file)))
	defer srv.Close()

	results, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if got, want := r.Line, "func needle() {}"; got != want {
		t.Errorf("line: got %q, want %q", got, want)
	}
	if got, want := r.Lineno, 3; got != want {
		t.Errorf("lineno: got %d, want %d", got, want)
	}
	if got, want := r.Line[r.Highlight[0]:r.Highlight[1]], "needle"; got != want {
		t.Errorf("highlight: got %q, want %q", got, want)
	}
	// the anchor must point at the line the tool reports, not one past it
	if got, want := r.FileURL, "https://example.com/testorg/repo/blob/main/src/f.go#L3"; got != want {
		t.Errorf("file URL: got %q, want %q", got, want)
	}
	if !strings.HasSuffix(r.FileURL, fmt.Sprintf("#L%d", r.Lineno)) {
		t.Errorf("file URL %q does not anchor line %d", r.FileURL, r.Lineno)
	}
}

// TestGithubFileURLAnchorMatchesLineno pins the anchor to the reported line
// number across several positions in the file.
func TestGithubFileURLAnchorMatchesLineno(t *testing.T) {
	file := "one\ntwo\nthree needle\nfour\nfive needle\n"
	srv := githubServer(t, codeResultJSON("src/f.go", "three needle", 6, 12),
		fileContents(base64File(file)))
	defer srv.Close()

	results, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("got no results")
	}
	for _, r := range results {
		if got, want := r.FileURL, fmt.Sprintf("https://example.com/testorg/repo/blob/main/src/f.go#L%d", r.Lineno); got != want {
			t.Errorf("file URL: got %q, want %q", got, want)
		}
	}
}

// TestGithubSearchLargeFile covers the >1MB case, where GitHub reports
// encoding "none" and omits the content entirely. The old code dereferenced
// Content unconditionally and panicked.
func TestGithubSearchLargeFile(t *testing.T) {
	srv := githubServer(t, codeResultJSON("src/f.go", "needle", 0, 6),
		fileContents(`{"type":"file","encoding":"none","name":"f.go","path":"src/f.go","size":2000000}`))
	defer srv.Close()

	_, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
	if err == nil {
		t.Fatal("got nil error, want a failure for an unfetchable file")
	}
	if !strings.Contains(err.Error(), "src/f.go") {
		t.Errorf("error %q does not name the file", err)
	}
}

// TestGithubSearchNullContent covers a file entry whose content field is
// absent, which also used to be dereferenced unconditionally.
func TestGithubSearchNullContent(t *testing.T) {
	srv := githubServer(t, codeResultJSON("src/f.go", "needle", 0, 6),
		fileContents(`{"type":"file","encoding":"base64","name":"f.go","path":"src/f.go"}`))
	defer srv.Close()

	if _, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{}); err == nil {
		t.Fatal("got nil error, want a failure for a file with no content")
	}
}

// TestGithubSearchDirectoryResponse covers GetContents returning a directory
// listing, where the file content is nil but there is no error.
func TestGithubSearchDirectoryResponse(t *testing.T) {
	srv := githubServer(t, codeResultJSON("src", "needle", 0, 6),
		fileContents(`[{"type":"file","name":"f.go","path":"src/f.go"}]`))
	defer srv.Close()

	_, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
	if err == nil {
		t.Fatal("got nil error, want a failure when the path is a directory")
	}
	if !strings.Contains(err.Error(), "no file content") {
		t.Errorf("got error %q, want it to mention missing file content", err)
	}
}

// TestGithubSearchSparseResult covers results where GitHub omits optional
// fields. The old code dereferenced them straight into Debugf arguments,
// which Go evaluates eagerly, so these panicked whether or not debug logging
// was enabled.
func TestGithubSearchSparseResult(t *testing.T) {
	for _, tt := range []struct {
		name string
		item string
	}{
		{
			name: "no sha or html_url",
			item: `{"name":"f.go","path":"src/f.go",
				"repository":{"name":"repo","owner":{"login":"testorg"}},
				"text_matches":[{"fragment":"needle","matches":[{"indices":[0,6]}]}]}`,
		},
		{
			name: "no repository",
			item: `{"name":"f.go","path":"src/f.go","sha":"x","html_url":"https://example.com/f",
				"text_matches":[{"fragment":"needle","matches":[{"indices":[0,6]}]}]}`,
		},
		{
			name: "no text match metadata",
			item: `{"name":"f.go","path":"src/f.go","sha":"x","html_url":"https://example.com/f",
				"repository":{"name":"repo","owner":{"login":"testorg"}},
				"text_matches":[{"fragment":"needle","matches":[{"indices":[0,6]}]}]}`,
		},
		{
			name: "match without indices",
			item: `{"name":"f.go","path":"src/f.go","sha":"x","html_url":"https://example.com/f",
				"repository":{"name":"repo","owner":{"login":"testorg"}},
				"text_matches":[{"fragment":"needle","matches":[{"text":"needle"}]}]}`,
		},
		{
			name: "empty text match list",
			item: `{"name":"f.go","path":"src/f.go","sha":"x","html_url":"https://example.com/f",
				"repository":{"name":"repo","owner":{"login":"testorg"}},"text_matches":[]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := githubServer(t, tt.item, fileContents(base64File("needle here\n")))
			defer srv.Close()
			// must not panic; an error is an acceptable outcome
			if _, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{}); err != nil {
				t.Logf("search returned error (acceptable): %v", err)
			}
		})
	}
}

// TestGithubDebugLoggingDoesNotPanic pins the reason the above crashed even
// with debug output disabled: the arguments are evaluated before Debugf is
// ever called.
func TestGithubDebugLoggingDoesNotPanic(t *testing.T) {
	old := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(old)

	srv := githubServer(t,
		`{"name":"f.go","path":"src/f.go",
			"repository":{"name":"repo","owner":{"login":"testorg"}},
			"text_matches":[{"fragment":"needle","matches":[{"indices":[0,6]}]}]}`,
		fileContents(base64File("needle here\n")))
	defer srv.Close()

	if _, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{}); err != nil {
		t.Logf("search returned error (acceptable): %v", err)
	}
}

// TestGithubCachesFileContent checks that a file with several matches is
// downloaded once rather than once per match. GitHub code search is rate
// limited hard, so the request count matters more than the wall clock.
func TestGithubCachesFileContent(t *testing.T) {
	var fetches int32
	item := `{
		"name":"f.go","path":"src/f.go","sha":"x",
		"html_url":"https://example.com/testorg/repo/blob/main/src/f.go",
		"repository":{"name":"repo","html_url":"https://example.com/testorg/repo","owner":{"login":"testorg"}},
		"text_matches":[
			{"fragment":"needle one","matches":[{"indices":[0,6]}]},
			{"fragment":"needle two","matches":[{"indices":[0,6]}]},
			{"fragment":"needle three","matches":[{"indices":[0,6]}]}
		]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/code"):
			searchPage(w, r, item, 0)
		case strings.Contains(r.URL.Path, "/contents/"):
			atomic.AddInt32(&fetches, 1)
			fmt.Fprint(w, base64File("needle one\nneedle two\nneedle three\n"))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	results, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := len(results), 3; got != want {
		t.Errorf("got %d results, want %d", got, want)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("got %d content fetches for one file with 3 matches, want 1", got)
	}
}

// TestGithubCacheIsPerFile makes sure the cache keys on the full path, so
// results in different files still fetch their own content.
func TestGithubCacheIsPerFile(t *testing.T) {
	var fetches int32
	seen := map[string]bool{}
	var mu sync.Mutex
	items := codeResultJSON("src/a.go", "needle", 0, 6) + "," + codeResultJSON("src/b.go", "needle", 0, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/code"):
			searchPage(w, r, items, 0)
		case strings.Contains(r.URL.Path, "/contents/"):
			atomic.AddInt32(&fetches, 1)
			mu.Lock()
			seen[r.URL.Path] = true
			mu.Unlock()
			fmt.Fprint(w, base64File("needle\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if _, err := newGithubBackend(t, srv.URL).Search(context.Background(), "needle", SearchOptions{}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Errorf("got %d content fetches for two distinct files, want 2", got)
	}
	if len(seen) != 2 {
		t.Errorf("got %d distinct paths fetched, want 2", len(seen))
	}
}
