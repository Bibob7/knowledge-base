# Development

## Working copy

`claude --plugin-dir .` loads the working copy as the plugin. `KB_FORCE_BUILD=1 scripts/kb --ensure` builds the
binary for the current version from source instead of downloading the release binary.

## Tests

```bash
go test ./...
```

CI runs the tests on every push and pull request, against pgvector too. Both backends pass the same contract tests
(`internal/store/store_contract_test.go`). SQLite always runs; Postgres only with a dedicated test database whose
tables are dropped first (create `kb_test` beforehand):

```bash
KB_TEST_DATABASE_URL='postgres://kb:kb@localhost:5433/kb_test?sslmode=disable' go test ./internal/store
```

The server instructions (`cmd/knowledge-base-mcp/instructions.md`) are cut by Claude Code after 4096 characters; a
test guards their size.

## Releases

Bump `version` in `.claude-plugin/plugin.json`, merge, then tag the same version:

```bash
git tag v0.7.0 && git push origin v0.7.0
```

The release workflow checks that tag and version match, runs the tests and publishes the binaries for macOS and
Linux (amd64, arm64) with `checksums.txt`. Users who update the plugin before the release exists build from source
(if Go is installed), so tag right after merging the version bump.
