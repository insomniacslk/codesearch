package codesearch

import (
	"bytes"
	"fmt"
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
	name              string
	indexFile         string
	linesBefore       int
	linesAfter        int
	caseInsensitive   bool
	searchInFilenames bool
}

func (g *Csearch) New(name string, params BackendParams) (Backend, error) {
	indexFile := params.GetString("index_file")
	if indexFile == nil {
		return nil, fmt.Errorf("missing 'index_file' parameter")
	}
	gl := Csearch{
		name:      name,
		indexFile: *indexFile,
	}
	return &gl, nil
}

func (g *Csearch) Name() string {
	return g.name
}

func (g *Csearch) Type() string {
	return BackendTypeCsearch
}

func (g *Csearch) SetCaseInsensitive(v bool) {
	g.caseInsensitive = v
}

func (g *Csearch) SetLinesBefore(n int) {
	g.linesBefore = n
}

func (g *Csearch) SetLinesAfter(n int) {
	g.linesAfter = n
}

func (g *Csearch) SetSearchInFilenames(v bool) {
	g.searchInFilenames = v
}

func removePathPrefix(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		s = s[len(prefix):]
		s = strings.TrimLeft(s, "/")
	}
	return s
}

func (g *Csearch) Search(searchString string, opts ...Opt) (Results, error) {
	for _, opt := range opts {
		opt(g)
	}
	pattern := "(?m)" + searchString
	if g.caseInsensitive {
		pattern = "(?i)" + pattern
	}
	ix := index.Open(g.indexFile)
	if g.searchInFilenames {
		// get all the file names instead of doing a search on the cindex
		logrus.Debugf("Searching in file names")
		re, err := goregexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
		}
		var results Results
		for _, indexedPath := range ix.Paths() {
			files := make(map[string]string)
			err = filepath.Walk(indexedPath, func(path string, info os.FileInfo, err error) error {
				shortName := removePathPrefix(info.Name(), path)
				if err == nil && re.MatchString(shortName) {
					files[shortName] = path
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("failed to walk the source tree: %w", err)
			}
			for name, path := range files {
				shortName := removePathPrefix(path, indexedPath)
				logrus.Debugf("indexPath=%s path=%s name=%s shortName=%s", indexedPath, path, name, shortName)
				result := Result{
					Backend:    g.Name(),
					Path:       shortName,
					RepoURL:    "file://" + indexedPath,
					FileURL:    "file://" + path,
					Owner:      "",
					RepoName:   indexedPath,
					IsFilename: true,
				}
				results = append(results, result)
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
	return g.toResult(pattern, ix, grep, post)
}

func (g *Csearch) toResult(pattern string, ix *index.Index, grep regexp.Grep, post []uint32) (Results, error) {
	var results Results
	re, err := goregexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile pattern for indexing: %w", err)
	}
	for _, fileid := range post {
		name := ix.Name(fileid)
		logrus.Debugf("fileid=%d name=%q", fileid, name)
		grep.File(name)
		b := grep.Stdout.(*bytes.Buffer)
		o := b.String()
		b.Reset()
		if o == "" {
			e := grep.Stderr.(*bytes.Buffer).String()
			if e != "" {
				return nil, fmt.Errorf("match reader failed: %s", e)
			}
			// no output, no error, just continue
			logrus.Debugf("Skipping empty file results")
			continue
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
			// find indexed path
			var indexedPath string
			for _, p := range ix.Paths() {
				if strings.HasPrefix(name, p) {
					indexedPath = p
					break
				}
			}
			if indexedPath == "" {
				return nil, fmt.Errorf("no indexed path found for %q", name)
			}
			var before, after []string
			if g.linesBefore > 0 || g.linesAfter > 0 {
				fullText, err := os.ReadFile(name)
				if err != nil {
					return nil, fmt.Errorf("failed to read file %q: %w", name, err)
				}
				lines := strings.Split(string(fullText), "\n")
				indexBefore := lineno - g.linesBefore
				if indexBefore < 0 {
					indexBefore = 0
				}
				indexAfter := lineno + g.linesAfter
				if indexAfter > len(lines) {
					indexAfter = len(lines)
				}
				before = lines[indexBefore:lineno]
				after = lines[lineno+1 : indexAfter+1]
			}
			// find start and end offsets for highlight
			offsets := re.FindAllStringIndex(line, -1)
			// TODO use the offsets after the first one, once multiple highlight
			// support is added to the Backend interface
			var start, end int
			if len(offsets) > 0 {
				start, end = offsets[0][0], offsets[0][1]
			}
			shortName := removePathPrefix(name, indexedPath)
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
				// TODO: IsFilename
				//IsFilename: true/false
			}
			results = append(results, result)
		}
	}
	return results, nil
}
