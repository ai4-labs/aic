# Examples

Runnable aic projects. They all work with today's `aic run` on macOS: no toolchains,
just shell scripts and the tools macOS ships in `/bin` and `/usr/bin`.

| Example | Shows |
|---|---|
| [hello](hello) | The smallest project, and what a sandboxed process sees |
| [csv-report](csv-report) | Reading a workspace; writing only where the operator's config allows |
| [sandbox](sandbox) | Running untrusted, AI-generated code with no network and no access to your files |

Run them from the repository root:

```sh
aic run examples/hello
aic run --config examples/csv-report/config.json --workspace examples/csv-report/data examples/csv-report
aic run --workspace examples/sandbox/job examples/sandbox
```

These examples are run by `go test ./cmd/aic` on macOS, so they stay working.
