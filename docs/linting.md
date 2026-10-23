# Go linting

The repository pins golangci-lint in `.golangci-lint-version`; CI reads that
file and applies the root `.golangci.yml` configuration. Pull-request CI reports
only findings introduced by the patch so existing lint debt does not block
unrelated changes. New findings in changed code still fail the check.

Install the pinned version locally, then run:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(cat .golangci-lint-version)
golangci-lint run --timeout=5m --new-from-rev=origin/feature/mobile-api
```

For a complete inventory, omit `--new-from-rev`; that reports baseline debt as
well as findings introduced by the current checkout.
