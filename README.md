# aic

A podman-like container image builder. `aic` builds OCI images from a
**Dockerfile** and pushes/pulls them to and from any OCI registry.

```sh
aic build -t hello examples/hello        # build ./examples/hello/Dockerfile
aic push hello registry.example.com/team/hello:1
aic pull registry.example.com/team/hello:1
```

## How it works

- **Native image assembly.** Base images, `COPY`/`ADD` layers and all metadata
  instructions (`ENV`, `LABEL`, `WORKDIR`, `USER`, `CMD`, ...) are handled by aic
  itself, with [go-containerregistry](https://github.com/google/go-containerregistry).
  A Dockerfile without `RUN` needs no container runtime at all.
- **`RUN` uses podman.** For each `RUN`, aic loads the image so far into podman,
  runs the command, exports the container filesystem and diffs it against the
  image to produce the new layer. On macOS this runs in the podman machine VM.
- **Local store.** Images live in an OCI image layout at
  `~/.local/share/aic/store` (override with `$AIC_STORE`).
- **Registry auth** comes from `~/.docker/config.json`, podman's
  `containers/auth.json`, `$REGISTRY_AUTH_FILE`, and credential helpers.
  Log in with `podman login` or `docker login`.

## Commands

```
aic build [flags] [CONTEXT]      build CONTEXT/Dockerfile (default context: .)
  -t, --tag NAME                 name the image (repeatable); unqualified names go under localhost/
  -f, --file PATH                Dockerfile path
  --build-arg KEY[=VALUE]        set a build argument (repeatable)
  --target STAGE                 build up to this stage
  --platform OS/ARCH[/VARIANT]   target platform (default linux/<host arch>)
  --pull                         always pull base images
  -q, --quiet                    print only the image digest

aic pull [--platform OS/ARCH] IMAGE
aic push IMAGE [DESTINATION]     IMAGE may be a name or a digest (prefix)
aic version
```

Flags may come before or after positional arguments. Shell completion:
`aic completion bash|zsh|fish|powershell`.

## Dockerfile support

| | |
|---|---|
| Instructions | `FROM` (incl. `--platform`, `AS`, `scratch`), `RUN`, `COPY`, `ADD`, `ARG`, `ENV`, `LABEL`, `WORKDIR`, `USER`, `EXPOSE`, `VOLUME`, `CMD`, `ENTRYPOINT`, `SHELL`, `STOPSIGNAL`, `HEALTHCHECK`, `ONBUILD`, `MAINTAINER` |
| Syntax | parser directives (`escape`), line continuations, exec and shell forms, heredocs (`RUN <<EOF`, `COPY <<EOF`), `$VAR` / `${VAR:-x}` / `${VAR:+x}` / `${VAR:?x}` |
| Multi-stage | `COPY --from=<stage, index or image>`; stages the target doesn't need are skipped |
| `COPY`/`ADD` | globs, `.dockerignore` (and `Dockerfile.dockerignore`), `--chown` (ids or names from the image), `--chmod`, `--link` (accepted) |
| `ADD` | local tar archives are extracted (plain, gzip, bzip2, zstd); `http(s)` URLs with optional `--checksum=sha256:...` |
| `RUN` | `--network=default\|none\|host` |
| Build args | `TARGETPLATFORM`, `TARGETOS`, `TARGETARCH`, `BUILDPLATFORM`, ... are predefined |

Not supported yet: `RUN --mount` (cache, secrets, ssh), git sources for `ADD`,
xz archives, `COPY --parents`/`--exclude`, and executing `ONBUILD` triggers of a
base image (aic warns and skips them).

## Requirements

- Go 1.25+ to build: `go build -o aic ./cmd/aic`
- For Dockerfiles with `RUN`: [podman](https://podman.io). On macOS,
  `podman machine init && podman machine start`.

## Limitations

- No build cache yet: every build runs every step.
- Each `RUN` streams the full image so far into podman, so builds on large base
  images with many `RUN` steps are slow.
- No `images`/`rmi`/`tag` commands or garbage collection of the store yet.
- On Linux, `RUN` also goes through podman. A native rootless executor is planned.
