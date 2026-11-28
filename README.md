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

### Environment variables

Every setting can also come from an environment variable named `CS_` followed
by the config key in upper case, with `.` replaced by `_`:

| Variable                         | Config key                     | Flag           |
|----------------------------------|--------------------------------|----------------|
| `CS_DEBUG`                       | `debug`                        | `--debug`      |
| `CS_STATS`                       | `stats`                        | `--stats`      |
| `CS_NO_COLOR`                    | `no_color`                     | `--no-color`   |
| `CS_DEFAULTS_LIMIT`              | `defaults.limit`               | `--limit`      |
| `CS_DEFAULTS_SORT`               | `defaults.sort`                | `--sort`       |
| `CS_DEFAULTS_BEFORE`             | `defaults.before`              | `--before`     |
| `CS_DEFAULTS_AFTER`              | `defaults.after`               | `--after`      |
| `CS_DEFAULTS_CONTEXT`            | `defaults.context`             | `--context`    |
| `CS_DEFAULTS_CASE_INSENSITIVE`   | `defaults.case_insensitive`    | `--case-insensitive` |
| `CS_DEFAULTS_SEARCH_IN_FILENAMES`| `defaults.search_in_filenames` | `--search-in-filenames` |
| `CS_DEFAULTS_MATCH_FILENAME`     | `defaults.match_filename`      | `--match-filename` |
| `CS_DEFAULTS_TIMEOUT`            | `defaults.timeout`             | `--timeout`    |
| `CS_DEFAULTS_JSON`               | `defaults.json`                | `--json`       |

A flag on the command line wins over the environment, which wins over the config
file. The standard `NO_COLOR` variable is honoured as well.

Backend names are matched without regard to case, because the config loader
lowercases them: `-b GitHub_Me` and `-b github_me` select the same backend.

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
| Search in file names     | ❌       | ❌     | ✅      |

Options that a backend cannot honour are rejected rather than ignored: asking
for `-i` or `-F` on a backend that does not implement them fails with a message
naming the backend and the flag, instead of silently returning results for a
different query.

Note that GitLab still reports matches that occur in a file name as part of an
ordinary search, it just has no file-name-only mode to select with `-F`.

Other general features:
* [ ] Common syntax for all backends
* [ ] Server-side search
* [ ] Custom colour scheme


`--match-filename`/`-f` filters results by path using an unanchored,
case-insensitive regexp, so `-f .go` matches anywhere in the path while
`-f '\.go$'` only matches the extension. Add an inline `(?-i)` to make it case
sensitive.

NOTE: there is no common syntax for searching, so for advanced queries you must know
each search engine's syntax and capabilities

## Tokens

Backend tokens do not have to be written into the config file. `token` accepts
a single environment variable reference, and `token_command` runs a command and
uses its output, which lets the secret live in a password manager:

```yaml
    params:
      token: ${GITHUB_TOKEN}
      # ...or, taking precedence over `token`:
      token_command: pass show github/codesearch
```

## GitHub file cache

The GitHub backend downloads every file that has a match, to show line numbers
and context. It keeps these files in `cs/github` in your user cache directory
(`~/.cache` on Linux), named after their git blob SHA, so a repeated search does
not download them again. An entry never goes out of date, because its name is a
checksum of its content.

The cache holds source code from every repository you search, private ones
included, and `cs` never deletes entries; remove the directory to reclaim the
space. To move the cache or turn it off, set `cache_dir` in the backend
parameters:

```yaml
    params:
      cache_dir: /some/other/place
      # ...or, to turn the cache off:
      cache_dir: ""
```

## Backends

Currently GitHub, GitLab and local search via
[google/codesearch](https://github.com/google/codesearch) are supported.
BitBucket support might be implemented in the future.

Note that for local search to work you must create (and keep up to date) a local
index using [`cindex`](https://github.com/google/codesearch/tree/master/cmd/cindex).
