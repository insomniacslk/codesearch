# codesearch

Command line code search tool that uses different backends, like
[GitHub](https://docs.github.com/en/rest/search/search),
[GitLab](https://docs.gitlab.com/ee/api/search.html),
and local search via
[`google/codesearch`](https://github.com/google/codesearch).

## Configuration

Create a file called `config.yml` in your configdir:
* on Linux, `~/.config/cs/config.yml`
* on macOS, `~/Library/Application Support/cs/config.yml`

Use the output of `cs config-example` to populate a sample config. Alternatively
see the file[`config.yml.example`](/cmd/cs/config.yml.example).

## Features

Feature matrix:

|                          | GitHub   | GitLab | Csearch |
|--------------------------|----------|--------|---------|
| Basic search             | ✅       | ✅     | ✅      |
| Regexp search            | ❌       | ❌     | ✅      |
| Colorized output         | ✅       | ✅     | ✅      |
| Highlight search pattern | ✅       | ✅     | ✅      |
| Limit to N results       | ✅       | ✅     | ✅      |
| Sorting                  | ✅       | ✅     | ✅      |
| Rate limiting            | ✅       | ❌     | N/A     |
| Response caching         | ❌       | ❌     | N/A     |
| Case sensitivity         | ❌       | ❌     | ✅      |
| Show context lines       | ✅       | max 3  | ✅      |
| Full file fetching       | ✅       | ❌     | ✅      |
| Search by file name      | ✅       | ✅     | ✅      |
| Search in file names     | ❌       | ✅     | ✅      |

Other general features:
* [ ] Common syntax for all backends
* [ ] Server-side search


NOTE: there is no common syntax for searching, so for advanced queries you must know
each search engine's syntax and capabilities

## Backends

Currently GitHub, GitLab and local search via
[google/codesearch](https://github.com/google/codesearch) are supported.
BitBucket support might be implemented in the future.

Note that for local search to work you must create (and keep up to date) a local
index using [`cindex`](https://github.com/google/codesearch/tree/master/cmd/cindex).
