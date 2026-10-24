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

- aic assembles images itself. Pulling base images, `COPY`, `ADD` and metadata
  instructions need no container runtime.
- Each `RUN` executes in a fresh rootless container that aic creates itself, and
  the changes become a new layer:
  - on Linux, directly on the host as your user;
  - on macOS, in a small Linux VM that aic boots in about a second.
- `RUN` has no network access unless you write `RUN --network=host`.
- Images are kept in `~/.aic/store`. Registry credentials come from
  `aic login`, `docker login` or `podman login`.

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

aic login [-u USER] [--password-stdin] REGISTRY   log in (credentials are verified first)
aic logout [-a] [REGISTRY]       remove credentials saved by aic login
aic images [-q] [--no-trunc]     list local images
aic rmi [-f] [-a] IMAGE...       remove images (a name untags; an ID removes all its names)
aic vm start|stop|status|logs|rm   manage the macOS build VM (see below)
aic pull [--platform OS/ARCH] IMAGE
aic push IMAGE [DESTINATION]     IMAGE may be a name or a digest (prefix)
aic version
```

Flags may come before or after positional arguments. Shell completion:
`aic completion bash|zsh|fish|powershell`.

## The build VM (macOS)

```sh
aic vm start [--cpus N] [--memory MiB]   # keep a VM running in the background
aic vm status                            # state, PID, size, uptime, number of RUNs
aic vm logs [-f]                         # the VM's console log
aic vm stop
aic vm rm [-f]                           # delete the cached kernel, initramfs and logs
```

The VM runs a [LinuxKit](https://github.com/linuxkit/linux) kernel, downloaded
once (~16 MB, pinned and checksummed); the guest agent is embedded in aic.
The background VM serves builds over a Unix socket (`aic.sock`, mode 0600) in
the VM directory. RUNs from concurrent builds are queued. Interrupting a build
doesn't affect the VM. On Linux there is no VM and `aic vm` reports so.

## Dockerfile support

All Dockerfile instructions are supported, including multi-stage builds,
heredocs, `.dockerignore`, and `ADD` of URLs and local archives.

Differences from Docker:

- `RUN` has no network unless you write `RUN --network=host`.
- Not supported yet: `RUN --mount`, `ADD` from git, xz archives,
  `COPY --parents`/`--exclude`, and `ONBUILD` triggers of base images.

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
| `AIC_STORE` | image store (default `~/.aic/store`) |
| `AIC_AUTH_FILE` | credentials saved by `aic login` (default `~/.aic/auth.json`, mode 0600; secrets go to a Docker credential helper such as `docker-credential-osxkeychain` if one is installed when the file is created) |
| `AIC_VM_DIR` | VM kernel, initramfs, logs and socket (default `~/.aic/vm`) |
| `AIC_VM_CPUS`, `AIC_VM_MEMORY` | VM size on macOS (default min(4, host CPUs) and 4096 MiB) |
| `SOURCE_DATE_EPOCH` | image creation time |

On Linux, install `newuidmap`/`newgidmap` (shadow-utils) and add an
`/etc/subuid` and `/etc/subgid` entry for your user so that `RUN` can use users
other than root; without them, chowning files to other users fails.

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
