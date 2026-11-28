package codesearch

import (
	"bytes"
	"context"
	"encoding/binary"
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
	// The table right before the trailer holds `fields` big-endian integers
	// of fieldSize bytes each. Those at the positions in offsets are where
	// the sections of the file start, in file order.
	fields    int
	fieldSize int
	offsets   []int
}

// indexMagicLen is the length of the header, the same in every format.
const indexMagicLen = 16

// indexFormats lists the formats the codesearch library can read. Version 1
// is written by cindex up to v1.2.0 and has five 4-byte offsets, version 2 by
// later releases and has eight 8-byte ones.
var indexFormats = []indexFormat{
	// path list, name list, posting lists, name index, posting list index
	{magic: "csearch index 1\n", trailer: "\ncsearch trailr\n", minSize: 16 + 5*4 + 16,
		fields: 5, fieldSize: 4, offsets: []int{0, 1, 2, 3, 4}},
	// the same sections, with the number of entries after the first three
	{magic: "csearch index 2\n", trailer: "\ncsearch trlr 2\n", minSize: 16 + 8*8 + 16,
		fields: 8, fieldSize: 8, offsets: []int{0, 2, 4, 6, 7}},
}

// openIndex validates the index before handing it to index.Open, which calls
// log.Fatal on anything it does not like and so would take the whole process
// down with "corrupt index" even when the file is simply not there. Checking
// the header, the size and the trailer catches missing, foreign and truncated
// files, and checking the section offsets catches a file whose table of
// contents is damaged. Corruption inside a section can still reach log.Fatal
// in the library; there is no way to recover from that short of re-parsing
// the whole format here.
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
	if err := format.checkOffsets(f, info.Size()); err != nil {
		return nil, fmt.Errorf("index %q is corrupt (%w), recreate it with cindex", path, err)
	}
	return index.Open(path), nil
}

// checkOffsets verifies that the sections of an index of this size start
// where a section can start: after the header, before the table itself, and
// in file order. The library takes these offsets on trust and slices the
// file with them.
func (format *indexFormat) checkOffsets(f io.ReaderAt, size int64) error {
	tableSize := int64(format.fields * format.fieldSize)
	tableStart := size - int64(len(format.trailer)) - tableSize
	table := make([]byte, tableSize)
	if _, err := f.ReadAt(table, tableStart); err != nil {
		return fmt.Errorf("cannot read the section table: %w", err)
	}
	previous := uint64(indexMagicLen)
	for _, field := range format.offsets {
		raw := table[field*format.fieldSize : (field+1)*format.fieldSize]
		var offset uint64
		if format.fieldSize == 4 {
			offset = uint64(binary.BigEndian.Uint32(raw))
		} else {
			offset = binary.BigEndian.Uint64(raw)
		}
		if offset < previous || offset > uint64(tableStart) {
			return fmt.Errorf("section offset %d is outside of %d..%d", offset, previous, tableStart)
		}
		previous = offset
	}
	return nil
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
	log := opts.logger()
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
		log.Debugf("Searching in file names")
		re, err := goregexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
		}
		return g.searchFilenames(ctx, ix, re)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regexp pattern: %w", err)
	}
	q := index.RegexpQuery(re.Syntax)
	post := ix.PostingQuery(q)
	return g.toResult(ctx, log, pattern, ix, post, opts)
}

// searchFilenames matches re against the name of every file in the index.
// The names come from the index itself, which is the list of what cindex
// indexed: walking the roots instead takes far longer on a big tree, and also
// reports files that the index does not hold, such as binaries and the
// content of .git directories.
func (g *Csearch) searchFilenames(ctx context.Context, ix *index.Index, re *goregexp.Regexp) (Results, error) {
	roots := indexRoots(ix)
	// the posting list of "every file" is the only exported way to learn how
	// many names the index holds
	count := len(ix.PostingQuery(&index.Query{Op: index.QAll}))
	var (
		results Results
		seen    int
	)
	for path := range ix.Names(0, count) {
		// a big index takes a while to go through, and this loop is the only
		// thing that can notice --timeout or Ctrl-C
		if seen++; seen%1024 == 1 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		name := path.String()
		indexedPath := longestRoot(roots, name)
		if indexedPath == "" {
			return nil, fmt.Errorf("no indexed path found for %q", name)
		}
		shortName := removePathPrefix(name, indexedPath)
		if !re.MatchString(shortName) {
			continue
		}
		results = append(results, Result{
			Backend:    g.Name(),
			Path:       shortName,
			RepoURL:    "file://" + indexedPath,
			FileURL:    "file://" + name,
			Owner:      "",
			RepoName:   indexedPath,
			IsFilename: true,
		})
	}
	return results, nil
}

// longestRoot returns the indexed root that name sits under, preferring the
// longest one so that nested roots resolve to the deeper one. It returns ""
// when there is none.
func longestRoot(roots []string, name string) string {
	var root string
	for _, p := range roots {
		if hasPathPrefix(name, p) && len(p) > len(root) {
			root = p
		}
	}
	return root
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

func (g *Csearch) toResult(ctx context.Context, log logrus.FieldLogger, pattern string, ix *index.Index, post []int, opts SearchOptions) (Results, error) {
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
			log.Debugf("fileid=%d name=%q", fileid, job.name)
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
			log.Warn(job.warning)
		}
		results = append(results, job.results...)
		for _, r := range job.results {
			opts.emit(r)
		}
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
	indexedPath := longestRoot(w.roots, name)
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
