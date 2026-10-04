# Contributing

1. Fork + clone.
2. `go test -race ./...` passes before opening a PR.
3. `gofmt -l .` is clean and `go vet ./...` is clean — CI checks both.
4. Commit messages: imperative mood (`fix: ...`, `feat: ...`), no strict prefix convention required.
5. `go run ./cmd/leakcheck` is clean: no private (RFC 1918) addresses, home-directory paths, credential-shaped strings or `docs/_*` working notes in tracked files. Use documentation values (`192.0.2.0/24`, `example.com`, `REPLACE-...`) in examples; a deliberate test fixture can carry `leakcheck:allow` on its line. To run it on every commit: `ln -sf ../../scripts/pre-commit .git/hooks/pre-commit`.
6. CI runs automatically on PRs — same checks as steps 2–5, plus `go build ./...`.

Small, focused PRs are easier to review than large ones. If you're planning something bigger than a bug fix, opening an issue first to discuss the approach is welcome but not required.
