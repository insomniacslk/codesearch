package codesearch

type Results []Result

type Result struct {
	Backend string `json:"backend"`
	Line    string `json:"line,omitempty"`
	Lineno  int    `json:"lineno,omitempty"`
	// IsFilename is true if the result matches just the file name, and false if
	// it matches the file content
	IsFilename bool          `json:"is_filename"`
	Context    ResultContext `json:"context"`
	Highlight  [2]int        `json:"highlight"`
	Path       string        `json:"path"`
	RepoURL    string        `json:"repo_url,omitempty"`
	FileURL    string        `json:"file_url,omitempty"`
	Owner      string        `json:"owner,omitempty"`
	RepoName   string        `json:"repo_name,omitempty"`
	Branch     string        `json:"branch,omitempty"`
}

type ResultContext struct {
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}
