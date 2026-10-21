# aic

A container format and runtime for AI developers, native on **macOS** and **Linux**.

- **Language-agnostic.** The runtime mounts content-addressed trees and runs a command.
  Python, Node, Go, Rust, or anything else comes from pluggable **toolchains**.
- **Reproducible.** Runtimes and dependencies are pinned by digest in `aic.lock`.
- **Sandboxed by default.** The operator supplies the config at run time; without one, everything is denied.
  An app can't grant itself permissions.
- **Native, no VM.** Linux uses rootless namespaces, cgroups v2, seccomp, and Landlock.
  macOS runs native binaries under Seatbelt.

> [!NOTE]
> **Status: early development.** Working today: `aic init`, `aic version`, the lock-keeping part of
> `aic build`, and `aic run` on **macOS** for projects without toolchains (it runs the project directory
> directly; bundles come in M1). See [`examples/`](examples) and `./demo.sh`. Other examples below show
> the **planned** interface defined in [`spec/format.md`](spec/format.md).

## Concepts

```
 aic.yaml ──lock/build──▶  App bundle  ──┐
                                         ├──▶  aic run  ──▶  sandboxed process
                    config.json  ────────┘   (operator-owned, never inside the bundle)
```

| | What | Who owns it |
|---|---|---|
| **App** | code, data, and per-platform toolchains | developer |
| **Config** | network, filesystem, limits | whoever *runs* the app |

Inside the container:

| Path | Contents |
|---|---|
| `$AIC_ROOT/<mount>` | app components (read-only) |
| `$AIC_ROOT/workspace` | host dir from `--workspace` (writable only if the config allows) |
| `$AIC_ROOT/scratch` | per-run writable scratch |

## Examples

### 1. Python: a batch job

```yaml
# aic.yaml
schemaVersion: 1
name: "report"
version: "0.1.0"

toolchains:
  python:
    version: "3.12"
    requirements: requirements.txt   # pandas, numpy, …

targets:
  - darwin-arm64
  - linux-amd64

include: ["src/**"]

entrypoint:
  cmd: [python, -m, src.report]
```

```sh
aic build              # creates or refreshes aic.lock when aic.yaml changes
aic build --locked     # in CI: fail instead if aic.lock is missing or stale

# No --config means deny-all: no network, nothing writable outside scratch.
aic run --workspace ./data report
```

Where results may be written is the operator's decision, made at run time:

```jsonc
// report-config.json
{ "filesystem": { "writable": ["$WORKSPACE/out"] } }
```

```sh
aic run --config report-config.json --workspace ./data report
```

### 2. TypeScript: an agent that may only reach one API

```yaml
# aic.yaml
schemaVersion: 1
name: "support-agent"
version: "1.2.0"

toolchains:
  node:
    version: "22"
    lockfile: package-lock.json

targets:
  - darwin-arm64
  - linux-amd64
  - linux-arm64

include: ["dist/**"]

entrypoint:
  cmd: [node, dist/agent.js]
```

```jsonc
// config.json: owned by whoever deploys the agent
{
  "network":    { "egress": ["api.openai.com:443"] },
  "filesystem": { "writable": ["$WORKSPACE/out"] },
  "maxMemory":  2147483648,
  "maxProcs":   32
}
```

```sh
aic run --config config.json --workspace ./tickets support-agent
# Any connection other than api.openai.com:443 is refused by the egress proxy.
# Writes anywhere except ./tickets/out and the per-run scratch dir fail with EPERM.
```

### 3. Go / Rust / C++: prebuilt binaries

Compiled languages need no special support. Build the binaries as usual, then ship one per platform:

```yaml
# aic.yaml
schemaVersion: 1
name: "ingest"
version: "0.4.0"

toolchains:
  prebuilt:
    darwin-arm64: dist/darwin-arm64      # contains bin/ingest
    linux-amd64:
      url: https://example.com/ingest-linux-amd64.tar.gz
      digest: sha256:9f2c…               # verified before use

targets:
  - darwin-arm64
  - linux-amd64

entrypoint:
  cmd: [ingest, --in, workspace/raw, --out, workspace/clean]
```

```jsonc
// ingest-config.json
{ "filesystem": { "writable": ["$WORKSPACE/clean"] }, "maxProcs": 16 }
```

```sh
aic run --config ingest-config.json --workspace ./data ingest
```

### 4. Running code an AI agent just wrote

Use `aic` as a disposable sandbox for untrusted, generated code. The app can be
nothing more than a toolchain with no code of its own:

```yaml
# aic.yaml
schemaVersion: 1
name: "py-sandbox"
version: "1"
toolchains:
  python:
    version: "3.12"
    requirements: sandbox-requirements.txt   # numpy, pandas, matplotlib
targets:
  - darwin-arm64
  - linux-amd64
entrypoint:
  cmd: [python]
```

```sh
# The agent writes ./job/solution.py and calls:
aic run --workspace ./job py-sandbox -- python workspace/solution.py
# Deny-all config: no network, cannot read ~/.ssh or anything else on the host,
# cannot write ./job (not granted), and dies when it hits default limits.

# Interactive debugging under the same restrictions:
aic shell --workspace ./job py-sandbox
```

### 5. Any other language: write a toolchain provider

A toolchain provider is an executable named `aic-toolchain-<name>` on `PATH`. It speaks a small
JSON protocol ([spec §8.1](spec/format.md#81-toolchains)). It pins inputs at
`lock` time and writes files at `materialize` time, and it tells the runtime which
`PATH` and environment variables its output needs:

```yaml
toolchains:
  julia:                 # runs aic-toolchain-julia
    version: "1.11"
    project: Project.toml
```

```json
{"components": [
  {"kind": "runtime", "name": "julia", "mount": "runtime/julia",
   "path": ["bin"], "env": {"JULIA_DEPOT_PATH": "${AIC_MOUNT}/depot"}}
]}
```

The runtime never learns what "Julia" is. It mounts the tree, prepends `bin` to
`PATH`, and sets `JULIA_DEPOT_PATH`.

## Getting started

```sh
aic version
aic init myproject      # writes myproject/aic.yaml
```

## Roadmap

| # | Milestone |
|---|---|
| M0 | Spec + skeleton ✅ |
| M1 | Content-addressed store, `build` / `inspect` / `gc` |
| M2 | macOS Seatbelt sandbox: `run` ✅ (project dirs, deny-all + writable grants), `shell`, limits |
| M3 | Linux namespace sandbox, `run` / `shell` |
| M4 | Toolchains: `python`, `node`, `prebuilt` + external provider protocol, resolved into `aic.lock` |
| M5 | Egress allowlist proxy |
| M6 | `push` / `pull`, signing |
