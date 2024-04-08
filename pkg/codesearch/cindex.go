package codesearch

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/codesearch/index"
	"github.com/google/codesearch/regexp"
)

// Cindex implements the Backend interface
type Cindex struct {
	name        string
	indexFile   string
	linesBefore int
	linesAfter  int
}

func (g *Cindex) New(name string, params BackendParams) (Backend, error) {
	indexFile := params.GetString("index_file")
	if indexFile == nil {
		return nil, fmt.Errorf("missing 'index_file' parameter")
	}
	gl := Cindex{
		name:      name,
		indexFile: *indexFile,
	}
	return &gl, nil
}

func (g *Cindex) Name() string {
	return g.name
}

func (g *Cindex) Type() string {
	return BackendTypeCindex
}

func (g *Cindex) SetLinesBefore(n int) {
	g.linesBefore = n
}

func (g *Cindex) SetLinesAfter(n int) {
	g.linesAfter = n
}

func (g *Cindex) Search(searchString string, opts ...Opt) (Results, error) {
	for _, opt := range opts {
		opt(g)
	}
	pattern := "(?m)" + searchString
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
	}
	ix := index.Open(g.indexFile)
	q := index.RegexpQuery(re.Syntax)
	post := ix.PostingQuery(q)
	return g.toResult(searchString, ix, grep, post)
}

func (g *Cindex) toResult(searchString string, ix *index.Index, grep regexp.Grep, post []uint32) (Results, error) {
	var (
		results Results
	)
	for _, fileid := range post {
		name := ix.Name(fileid)
		grep.File(name)
		o := grep.Stdout.(*bytes.Buffer).String()
		// FIXME: fork and patch google/codesearch to write results to a struct.
		// Reason: the index package of google/codesearch prints directly to
		// stdout/stderr rather than saving the values in a struct, so I have to
		// parse the output splitting by `:`. However if a file name contains a
		// `:`, the split is wrong.
		if o == "" {
			e := grep.Stderr.(*bytes.Buffer).String()
			if e != "" {
				return nil, fmt.Errorf("match reader failed: %s", e)
			}
			// no output, no error, just continue
			continue
		}
		parts := strings.SplitN(o, ":", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("malformed result line: has less than 3 components. Line: %q", o)
		}
		filename := parts[0]
		lineno64, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid line number: %w", err)
		}
		lineno := int(lineno64)
		line := parts[2]
		newlineIdx := strings.Index(line, "\n")
		if newlineIdx != -1 {
			line = line[:newlineIdx]
		}
		// find indexed path
		var indexedPath string
		for _, p := range ix.Paths() {
			if strings.HasPrefix(filename, p) {
				indexedPath = p
				break
			}
		}
		if indexedPath == "" {
			return nil, fmt.Errorf("no indexed path found for %q", filename)
		}
		var before, after []string
		if g.linesBefore > 0 || g.linesAfter > 0 {
			fullText, err := os.ReadFile(filename)
			if err != nil {
				return nil, fmt.Errorf("failed to read file %q: %w", filename, err)
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
		result := Result{
			Backend:  g.Name(),
			Path:     filename,
			RepoURL:  "file://" + indexedPath,
			FileURL:  "file://" + filename,
			Owner:    "",
			RepoName: indexedPath,
			Branch:   "",
			Context:  ResultContext{Before: before, After: after},
			Lineno:   lineno,
			Line:     line,
			// TODO: Highlight
			//Highlight: [2]int{...},
			// TODO: IsFilename
			//IsFilename: true/false
		}
		results = append(results, result)
	}
	return results, nil
}
