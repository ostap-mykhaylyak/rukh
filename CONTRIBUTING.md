# Contributing

## Ground rules

- Standard library first: the only third-party dependencies are
  `gopkg.in/yaml.v3` and `github.com/fsnotify/fsnotify`. Anything else
  needs a very good reason.
- The binary must stay static: `CGO_ENABLED=0`, no cgo, no runtime
  dependencies.
- No config, logging or CLI frameworks.
- rukh is an optimizer, not a monitoring system: no dashboard, no
  metrics export, no external storage. The traffic model lives in
  memory and nowhere else.

## Before opening a PR

```sh
make fmt    # gofmt -s
make vet
make test   # race detector on
```

CI enforces `go mod tidy`, gofmt, vet and tests on every push.

## Conventions

- Commands: the service verbs (`start`, `stop`, `reload`, `restart`,
  `status`) are bare words; everything else (`--init`, `--purge`,
  `--check-config`, `--version`) takes a leading `--`.
- Config: every field has a production default, so an empty
  `config.yaml` must work; invalid list entries are skipped with a
  warning, never fatal.
- Logs: JSON via `log/slog`, one stream per concern, rotation
  delegated to logrotate (SIGHUP reopens the files).
- The request path never blocks on the learning machinery: observations
  are queued and dropped under pressure.
- Stable public surface: the JSON field names of `rukh status --json`
  must not change between versions.
