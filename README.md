# codesearch

Command line code search tool that uses different backends, like
[GitHub](https://docs.github.com/en/rest/search/search),
[GitLab](https://docs.gitlab.com/ee/api/search.html),
and soon local search via
[`google/codesearch`](https://github.com/google/codesearch).

## Configuration

Create a file called `config.yml` in your configdir:
* on Linux, `~/.config/cs/config.yml`
* on macOS, `~/Library/Application Support/cs/config.yml`

And use the content of the provided [`config.yml.example`](/config.yml.example) with your own
repository configuration and tokens.

## Features

Feature matrix:

|                      | GitHub   | GitLab | Local |
| Basic search         | ✅       | ✅     | ❌    |
| Limit to N results   | ❌       | ❌     | ❌    |
| Rate limiting        | ✅       | ❌     | ❌    |
| Response caching     | ❌       | ❌     | ❌    |
| Full file fetching   | ✅       | ❌     | ❌    |
| Search by file name  | ✅       | ✅     | ❌    |
| Search in file names | ❌       | ✅     | ❌    |

Other general features:
* [x] Colorized output
* [x] Search context (lines before/after)
* [ ] Common syntax for all backends
* [ ] Server-side search
