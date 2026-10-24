# aic

A podman-like container image builder. `aic` builds OCI images from a
**Dockerfile** and pushes/pulls them to and from any OCI registry. It is a
single binary: no daemon, no podman or Docker, no root.

```sh
aic build -t hello examples/hello        # build ./examples/hello/Dockerfile
aic push hello registry.example.com/team/hello:1
aic pull registry.example.com/team/hello:1
```

## How it works

- **Native image assembly.** Base images, `COPY`/`ADD` layers and all metadata
  instructions (`ENV`, `LABEL`, `WORKDIR`, `USER`, `CMD`, ...) are handled by aic
  itself, with [go-containerregistry](https://github.com/google/go-containerregistry).
- **Built-in rootless `RUN`.** Each `RUN` gets a fresh container: aic re-executes
  itself in new user, mount, PID, UTS, IPC and network namespaces, unpacks the
  image into a tmpfs, runs the command, and diffs the result against the image
  to produce the new layer.
  - **Linux:** runs directly on the host as your user. With `newuidmap`/`newgidmap`
    (shadow-utils) and an `/etc/subuid`/`/etc/subgid` entry, the full ID range is
    mapped; otherwise only root is, and chowning to other users fails.
  - **macOS:** aic boots a small Linux VM with Apple's Virtualization framework
    (about a second) and runs the same container code in it. The kernel is a
    [LinuxKit](https://github.com/linuxkit/linux) kernel, downloaded once (~16 MB,
    pinned and checksummed); the guest agent is embedded in aic.
- **Network:** `RUN` is isolated (loopback only) unless you use
  `RUN --network=host`, which shares the host's network (the VM's NAT on macOS).
- **Local store.** Images live in an OCI image layout at
  `~/.local/share/aic/store` (override with `$AIC_STORE`).
- **Registry auth** comes from `~/.docker/config.json`, podman's
  `containers/auth.json`, `$REGISTRY_AUTH_FILE`, and credential helpers.
  Log in with `docker login`, `podman login`, or write that file yourself.

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

aic images [-q] [--no-trunc]     list local images
aic rmi [-f] [-a] IMAGE...       remove images (a name untags; an ID removes all its names)
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
| `RUN` | `--network=none\|host` (`default` means `none`) |
| Build args | `TARGETPLATFORM`, `TARGETOS`, `TARGETARCH`, `BUILDPLATFORM`, ... are predefined |

Not supported yet: `RUN --mount` (cache, secrets, ssh), git sources for `ADD`,
xz archives, `COPY --parents`/`--exclude`, and executing `ONBUILD` triggers of a
base image (aic warns and skips them).

## Building

```sh
make            # builds ./aic
make install    # installs it to $(go env GOPATH)/bin (override with BINDIR=...)
```

On macOS, `make` cross-compiles the Linux guest (`cmd/aic-guest`), embeds it,
and signs `aic` with the `com.apple.security.virtualization` entitlement
(ad hoc). A plain `go build` works for everything except `RUN`. Requires Go 1.25+
and, on macOS, the Xcode command line tools (cgo); tested on macOS 15.

| Variable | |
|---|---|
| `AIC_STORE` | image store (default `~/.local/share/aic/store`) |
| `AIC_VM_DIR` | VM kernel, initramfs and `console.log` (default `~/.local/share/aic/vm`) |
| `AIC_VM_CPUS`, `AIC_VM_MEMORY` | VM size on macOS (default 4 CPUs, 4096 MiB) |
| `SOURCE_DATE_EPOCH` | image creation time |

On Ubuntu 23.10 and later, AppArmor blocks unprivileged user namespaces by
default. Allow them with `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`
or an AppArmor profile for aic.

## Limitations

- No build cache yet: every build runs every step.
- Each `RUN` unpacks the full image so far into a tmpfs, so memory bounds
  how large an image can be built (on macOS, the VM's memory).
- The macOS VM is arm64 only (Apple silicon), and `RUN` uses the host's
  architecture: no emulation for other `--platform`s yet.
- No `tag` command yet.
