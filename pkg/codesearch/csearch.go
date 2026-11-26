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
	"strconv"
	"strings"

	"github.com/google/codesearch/index"
	"github.com/google/codesearch/regexp"
	"github.com/sirupsen/logrus"
)

// Csearch implements the Backend interface
type Csearch struct {
	name      string
	indexFile string
}

func (g *Csearch) New(name string, params BackendParams) (Backend, error) {
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

func (g *Csearch) Type() string {
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

// indexMagic and indexTrailer are the header and trailer cindex writes at the
// start and end of an index file. Five 4-byte section offsets sit right before
// the trailer, so no real index is shorter than indexMinSize.
const (
	indexMagic   = "csearch index 1\n"
	indexTrailer = "\ncsearch trailr\n"
	indexMinSize = len(indexMagic) + 5*4 + len(indexTrailer)
)

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
	defer f.Close()

	header := make([]byte, len(indexMagic))
	if _, err := io.ReadFull(f, header); err != nil || string(header) != indexMagic {
		return nil, fmt.Errorf("%q is not a codesearch index, recreate it with cindex", path)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat index %q: %w", path, err)
	}
	if info.Size() < int64(indexMinSize) {
		return nil, fmt.Errorf("index %q is truncated, recreate it with cindex", path)
	}
	trailer := make([]byte, len(indexTrailer))
	if _, err := f.ReadAt(trailer, info.Size()-int64(len(indexTrailer))); err != nil || string(trailer) != indexTrailer {
		return nil, fmt.Errorf("index %q is truncated, recreate it with cindex", path)
	}
	return index.Open(path), nil
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
		for _, indexedPath := range ix.Paths() {
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
	stdout, stderr := bytes.Buffer{}, bytes.Buffer{}
	grep := regexp.Grep{
		Stdout: &stdout,
		Stderr: &stderr,
		Regexp: re,
		N:      true, // need lineno
		H:      true, // do not print file names
	}
	q := index.RegexpQuery(re.Syntax)
	post := ix.PostingQuery(q)
	return g.toResult(ctx, pattern, ix, grep, post, opts)
}

func (g *Csearch) toResult(ctx context.Context, pattern string, ix *index.Index, grep regexp.Grep, post []uint32, opts SearchOptions) (Results, error) {
	var results Results
	re, err := goregexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
	}
	for _, fileid := range post {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if opts.Limit > 0 && uint(len(results)) >= opts.Limit {
			break
		}
		name := ix.Name(fileid)
		logrus.Debugf("fileid=%d name=%q", fileid, name)
		grep.File(name)
		b := grep.Stdout.(*bytes.Buffer)
		o := b.String()
		b.Reset()
		eb := grep.Stderr.(*bytes.Buffer)
		e := strings.TrimSpace(eb.String())
		eb.Reset()
		if o == "" {
			if e != "" {
				// the index is a snapshot, so files that were deleted or
				// made unreadable since cindex ran are the normal case and
				// must not cost the hits from every other file
				logrus.Warnf("Skipping %q: %s", name, e)
				continue
			}
			// no output, no error, just continue
			logrus.Debugf("Skipping empty file results")
			continue
		}
		// find the indexed path this file sits under, preferring the
		// longest match so nested indexed roots resolve to the deeper one
		var indexedPath string
		for _, p := range ix.Paths() {
			if hasPathPrefix(name, p) && len(p) > len(indexedPath) {
				indexedPath = p
			}
		}
		if indexedPath == "" {
			return nil, fmt.Errorf("no indexed path found for %q", name)
		}
		shortName := removePathPrefix(name, indexedPath)
		// read the file once for all of its matches, not once per match
		var fileLines []string
		if opts.LinesBefore > 0 || opts.LinesAfter > 0 {
			fullText, err := os.ReadFile(name)
			if err != nil {
				logrus.Warnf("Cannot read %q for context, showing the match alone: %v", name, err)
			} else {
				fileLines = strings.Split(string(fullText), "\n")
			}
		}
		olines := strings.Split(o, "\n")
		for _, oline := range olines {
			if oline == "" {
				// skip empty lines reulting from the newline split
				continue
			}
			parts := strings.SplitN(oline, ":", 2)
			if len(parts) < 2 {
				return nil, fmt.Errorf("malformed result line: has fewer than 2 components. Line: %q", oline)
			}
			lineno64, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid line number: %w", err)
			}
			lineno := int(lineno64)
			line := parts[1]
			newlineIdx := strings.Index(line, "\n")
			if newlineIdx != -1 {
				line = line[:newlineIdx]
			}
			var before, after []string
			if fileLines != nil {
				before, after = contextLines(fileLines, lineno, opts.LinesBefore, opts.LinesAfter)
			}
			// find start and end offsets for highlight
			offsets := re.FindAllStringIndex(line, -1)
			// TODO use the offsets after the first one, once multiple highlight
			// support is added to the Backend interface
			var start, end int
			if len(offsets) > 0 {
				start, end = offsets[0][0], offsets[0][1]
			}
			result := Result{
				Backend:   g.Name(),
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
			}
			results = append(results, result)
		}
	}
	return results, nil
}
