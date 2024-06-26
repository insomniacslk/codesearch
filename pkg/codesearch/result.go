package codesearch

type Results []Result

type Result struct {
	Backend string
	Line    string
	Lineno  int
	// IsFilename is true if the result matches just the file name, and false if
	// it matches the file content
	IsFilename bool
	Context    ResultContext
	Highlight  [2]int
	Path       string
	RepoURL    string
	FileURL    string
	Owner      string
	RepoName   string
	Branch     string
}

type ResultContext struct {
	Before []string
	After  []string
}
