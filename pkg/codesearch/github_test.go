package codesearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v60/github"
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

	if _, err := newGithubBackend(t, srv.URL).Search("needle"); err != nil {
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
		_, err := newGithubBackend(t, srv.URL).Search("needle")
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
	_, err := g.Search("needle", WithTimeout(100*time.Millisecond))
	if err == nil {
		t.Fatal("got nil error, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("took %s, want it to stop at the timeout", elapsed)
	}
}
