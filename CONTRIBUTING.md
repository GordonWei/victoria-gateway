# Contributing

1. Fork + clone.
2. `go test -race ./...` passes before opening a PR.
3. `gofmt -l .` is clean and `go vet ./...` is clean — CI checks both.
4. Commit messages: imperative mood (`fix: ...`, `feat: ...`), no strict prefix convention required.
5. CI runs automatically on PRs — same checks as steps 2–3, plus `go build ./...`.

Small, focused PRs are easier to review than large ones. If you're planning something bigger than a bug fix, opening an issue first to discuss the approach is welcome but not required.
