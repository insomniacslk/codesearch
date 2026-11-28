package codesearch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goregexp "regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/google/codesearch/index"
	"github.com/google/codesearch/regexp"
	"github.com/sirupsen/logrus"
)

// Csearch implements the Backend interface
type Csearch struct {
	name      string
	indexFile string
}

func NewCsearch(name string, params BackendParams) (*Csearch, error) {
	indexFile := params.GetString("index_file")
	if indexFile == nil {
		return nil, fmt.Errorf("missing 'index_file' parameter")
	}
	expanded, err := expandHome(*indexFile)
	if err != nil {
		return nil, fmt.Errorf("failed to expand path %q: %w", *indexFile, err)
	}
	gl := Csearch{
		name:      name,
		indexFile: expanded,
	}
	return &gl, nil
}

// expandHome resolves a leading ~ against the current user's home directory.
// os.UserHomeDir does not do this itself, and ~user/ is not supported: there
// is no portable way to look up another user's home directory.
func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return "", errors.New("cannot expand user-specific home directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to look up home directory: %w", err)
	}
	return filepath.Join(home, path[1:]), nil
}

func (g *Csearch) Name() string {
	return g.name
}

func (g *Csearch) Type() BackendType {
	return BackendTypeCsearch
}

func (g *Csearch) Supports(c Capability) bool {
	switch c {
	case CapCaseInsensitive, CapSearchInFilenames:
		return true
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// contextLines returns the lines surrounding the match at lineno, which is
// 1-based. Every index is clamped to the file, so a match near either end of
// the file yields a shorter context rather than going out of range.
func contextLines(lines []string, lineno, before, after int) ([]string, []string) {
	n := len(lines)
	idx := lineno - 1
	beforeStart := clamp(idx-before, 0, n)
	beforeEnd := clamp(idx, beforeStart, n)
	afterStart := clamp(idx+1, 0, n)
	afterEnd := clamp(idx+1+after, afterStart, n)
	return lines[beforeStart:beforeEnd], lines[afterStart:afterEnd]
}

// hasPathPrefix reports whether s is prefix or sits underneath it, comparing
// whole path components so that "/src/repo" does not match "/src/repo2".
func hasPathPrefix(s, prefix string) bool {
	trimmed := strings.TrimRight(prefix, "/")
	if trimmed == "" {
		// a root prefix trims to nothing, but it still contains every
		// absolute path; an empty prefix contains nothing
		return prefix != "" && strings.HasPrefix(s, "/")
	}
	return s == trimmed || strings.HasPrefix(s, trimmed+"/")
}

func removePathPrefix(s, prefix string) string {
	if !hasPathPrefix(s, prefix) {
		return s
	}
	return strings.TrimPrefix(strings.TrimPrefix(s, strings.TrimRight(prefix, "/")), "/")
}

// indexFormat describes one version of the cindex file format: the header and
// trailer cindex writes at the start and end of the file, and the size of the
// section offsets that sit right before the trailer. No real index is shorter
// than minSize.
type indexFormat struct {
	magic   string
	trailer string
	minSize int64
}

// indexMagicLen is the length of the header, the same in every format.
const indexMagicLen = 16

// indexFormats lists the formats the codesearch library can read. Version 1
// is written by cindex up to v1.2.0 and has five 4-byte offsets, version 2 by
// later releases and has eight 8-byte ones.
var indexFormats = []indexFormat{
	{magic: "csearch index 1\n", trailer: "\ncsearch trailr\n", minSize: 16 + 5*4 + 16},
	{magic: "csearch index 2\n", trailer: "\ncsearch trlr 2\n", minSize: 16 + 8*8 + 16},
}

// openIndex validates the index before handing it to index.Open, which calls
// log.Fatal on anything it does not like and so would take the whole process
// down with "corrupt index" even when the file is simply not there. Checking
// the header, the size and the trailer catches missing, foreign and truncated
// files. Deeper corruption (bad section offsets) can still reach log.Fatal in
// the library; there is no way to recover from that short of re-parsing the
// whole format here.
func openIndex(path string) (*index.Index, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("index %q does not exist, create it with cindex", path)
		}
		return nil, fmt.Errorf("cannot read index %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	header := make([]byte, indexMagicLen)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("%q is not a codesearch index, recreate it with cindex", path)
	}
	var format *indexFormat
	for i := range indexFormats {
		if string(header) == indexFormats[i].magic {
			format = &indexFormats[i]
		}
	}
	if format == nil {
		if strings.HasPrefix(string(header), "csearch index ") {
			return nil, fmt.Errorf("index %q has the unsupported format version %q, recreate it with a cindex that this build of cs supports",
				path, strings.TrimSpace(strings.TrimPrefix(string(header), "csearch index ")))
		}
		return nil, fmt.Errorf("%q is not a codesearch index, recreate it with cindex", path)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat index %q: %w", path, err)
	}
	if info.Size() < format.minSize {
		return nil, fmt.Errorf("index %q is truncated, recreate it with cindex", path)
	}
	trailer := make([]byte, len(format.trailer))
	if _, err := f.ReadAt(trailer, info.Size()-int64(len(trailer))); err != nil || string(trailer) != format.trailer {
		return nil, fmt.Errorf("index %q is truncated, recreate it with cindex", path)
	}
	return index.Open(path), nil
}

// indexRoots returns the directories that were passed to cindex.
func indexRoots(ix *index.Index) []string {
	var roots []string
	for p := range ix.Roots().All() {
		roots = append(roots, p.String())
	}
	return roots
}

func (g *Csearch) Search(ctx context.Context, searchString string, opts SearchOptions) (Results, error) {
	ctx, cancel := searchContext(ctx, opts.Timeout)
	defer cancel()

	pattern := "(?m)" + searchString
	if opts.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	ix, err := openIndex(g.indexFile)
	if err != nil {
		return nil, err
	}
	if opts.SearchInFilenames {
		// get all the file names instead of doing a search on the cindex
		logrus.Debugf("Searching in file names")
		re, err := goregexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
		}
		var results Results
		for _, indexedPath := range indexRoots(ix) {
			// results are appended as the walk visits them, which is lexical
			// order, so that --limit picks the same subset on every run
			err = filepath.Walk(indexedPath, func(path string, info os.FileInfo, err error) error {
				// a big tree takes a long time to walk, and the walk is the
				// only thing that can notice --timeout or Ctrl-C
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				// info is nil whenever err is set, which happens for
				// directories that cannot be read. Skip them rather than
				// aborting the whole walk.
				if err != nil {
					logrus.Debugf("Skipping %q: %v", path, err)
					return nil
				}
				if info.IsDir() {
					return nil
				}
				shortName := removePathPrefix(path, indexedPath)
				if !re.MatchString(shortName) {
					return nil
				}
				logrus.Debugf("indexPath=%s path=%s shortName=%s", indexedPath, path, shortName)
				results = append(results, Result{
					Backend:    g.Name(),
					Path:       shortName,
					RepoURL:    "file://" + indexedPath,
					FileURL:    "file://" + path,
					Owner:      "",
					RepoName:   indexedPath,
					IsFilename: true,
				})
				return nil
			})
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				return nil, fmt.Errorf("failed to walk the source tree: %w", err)
			}
		}
		return results, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regexp pattern: %w", err)
	}
	q := index.RegexpQuery(re.Syntax)
	post := ix.PostingQuery(q)
	return g.toResult(ctx, pattern, ix, post, opts)
}

// csearchJob is the grep of one candidate file. The fields below done are set
// before done is closed.
type csearchJob struct {
	name string
	done chan struct{}

	results Results
	// warning says why the file was skipped or shown without context; it is
	// logged by the consumer so that warnings come out in file order
	warning string
	err     error
}

// csearchGrepper greps files for one pattern. The codesearch matcher keeps
// state between calls, so each worker needs its own.
type csearchGrepper struct {
	backend string
	roots   []string
	opts    SearchOptions
	// locate finds the highlight within a matching line
	locate *goregexp.Regexp
	grep   regexp.Grep
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newCsearchGrepper(backend, pattern string, locate *goregexp.Regexp, roots []string, opts SearchOptions) (*csearchGrepper, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regexp pattern: %w", err)
	}
	w := &csearchGrepper{backend: backend, roots: roots, opts: opts, locate: locate}
	w.grep = regexp.Grep{
		Stdout: &w.stdout,
		Stderr: &w.stderr,
		Regexp: re,
		N:      true, // need lineno
		H:      true, // do not print file names
	}
	return w, nil
}

func (g *Csearch) toResult(ctx context.Context, pattern string, ix *index.Index, post []int, opts SearchOptions) (Results, error) {
	locate, err := goregexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
	}
	roots := indexRoots(ix)
	workers := max(1, min(runtime.GOMAXPROCS(0), len(post)))
	greppers := make([]*csearchGrepper, workers)
	for i := range greppers {
		if greppers[i], err = newCsearchGrepper(g.Name(), pattern, locate, roots, opts); err != nil {
			return nil, err
		}
	}

	// The index only narrows the search down to candidate files; each one
	// still has to be read and matched, which is where the time goes. Workers
	// grep the candidates in parallel, and the loop at the bottom collects
	// them in index order, so the results are the same as from a serial run.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	jobs := make(chan *csearchJob)
	// ordered bounds how far the workers run ahead of the collecting loop,
	// which matters when --limit ends the search early
	ordered := make(chan *csearchJob, 2*workers)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		defer close(ordered)
		for _, fileid := range post {
			job := &csearchJob{name: ix.Name(fileid).String(), done: make(chan struct{})}
			logrus.Debugf("fileid=%d name=%q", fileid, job.name)
			select {
			case ordered <- job:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- job:
			case <-ctx.Done():
				// nobody will run it, but the collecting loop may wait on it
				close(job.done)
				return
			}
		}
	}()
	for _, w := range greppers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() == nil {
					job.results, job.warning, job.err = w.file(job.name)
				}
				close(job.done)
			}
		}()
	}

	var results Results
	for job := range ordered {
		if opts.Limit > 0 && uint(len(results)) >= opts.Limit {
			break
		}
		select {
		case <-job.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if job.err != nil {
			return nil, job.err
		}
		if job.warning != "" {
			logrus.Warn(job.warning)
		}
		results = append(results, job.results...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// file greps one file and returns its results, or a warning when the file
// had to be skipped or is shown without context.
func (w *csearchGrepper) file(name string) (results Results, warning string, err error) {
	w.grep.File(name)
	o := w.stdout.String()
	w.stdout.Reset()
	e := strings.TrimSpace(w.stderr.String())
	w.stderr.Reset()
	if o == "" {
		if e != "" {
			// the index is a snapshot, so files that were deleted or
			// made unreadable since cindex ran are the normal case and
			// must not cost the hits from every other file
			return nil, fmt.Sprintf("Skipping %q: %s", name, e), nil
		}
		// no output, no error: the index gave a false positive
		return nil, "", nil
	}
	// find the indexed path this file sits under, preferring the
	// longest match so nested indexed roots resolve to the deeper one
	var indexedPath string
	for _, p := range w.roots {
		if hasPathPrefix(name, p) && len(p) > len(indexedPath) {
			indexedPath = p
		}
	}
	if indexedPath == "" {
		return nil, "", fmt.Errorf("no indexed path found for %q", name)
	}
	shortName := removePathPrefix(name, indexedPath)
	// read the file once for all of its matches, not once per match
	var fileLines []string
	if w.opts.LinesBefore > 0 || w.opts.LinesAfter > 0 {
		fullText, err := os.ReadFile(name)
		if err != nil {
			warning = fmt.Sprintf("Cannot read %q for context, showing the match alone: %v", name, err)
		} else {
			fileLines = strings.Split(string(fullText), "\n")
		}
	}
	for _, oline := range strings.Split(o, "\n") {
		if oline == "" {
			// skip empty lines reulting from the newline split
			continue
		}
		parts := strings.SplitN(oline, ":", 2)
		if len(parts) < 2 {
			return nil, "", fmt.Errorf("malformed result line: has fewer than 2 components. Line: %q", oline)
		}
		lineno64, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid line number: %w", err)
		}
		lineno := int(lineno64)
		line := parts[1]
		var before, after []string
		if fileLines != nil {
			before, after = contextLines(fileLines, lineno, w.opts.LinesBefore, w.opts.LinesAfter)
		}
		// find start and end offsets for highlight
		// TODO use the offsets after the first one, once multiple highlight
		// support is added to the Backend interface
		var start, end int
		if loc := w.locate.FindStringIndex(line); loc != nil {
			start, end = loc[0], loc[1]
		}
		results = append(results, Result{
			Backend:   w.backend,
			Path:      shortName,
			RepoURL:   "file://" + indexedPath,
			FileURL:   "file://" + name,
			Owner:     "",
			RepoName:  indexedPath,
			Branch:    "",
			Context:   ResultContext{Before: before, After: after},
			Lineno:    lineno,
			Line:      line,
			Highlight: [2]int{start, end},
		})
	}
	return results, warning, nil
}
