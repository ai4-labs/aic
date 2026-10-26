# aic

Working with container images is harder than it should be, especially for AI
developers: the tooling is complex and fragmented, and image formats don't
compose well. aic is an experiment in doing it differently.

Goals: aic builds OCI images from a **Dockerfile** (for now), runs them in isolated containers,
and pushes and pulls them to and from any OCI registry. It is a single binary
with no daemon. On Linux it is rootless and doesn't require a runtime such as runc; on
macOS it uses Apple's built-in virtualization directly.

> [!WARNING]
> aic is experimental. It has known limitations (see below), which we expect
> to address over time.

```sh
aic build -t claude-code examples/claude-code
aic run -it -w . --network=host -e ANTHROPIC_API_KEY claude-code
```

## Install

```sh
go install github.com/ai4-labs/aic/cmd/aic@latest
```

Requires Go 1.26+ and, on macOS, the Xcode command line tools. On macOS, the
first build or run signs aic for Virtualization.framework and compiles the
VM's Linux guest from the same source, once.

## How it works

- aic assembles images itself. Pulling base images, `COPY`, `ADD` and metadata
  instructions need no container runtime.
- Each `RUN` executes in a fresh rootless container that aic creates itself, and
  the changes become a new layer:
  - on Linux, directly on the host as your user;
  - on macOS, in a small Linux VM that aic boots in about a second.
- `RUN` shares the host's network (on macOS, the VM's). Use
  `RUN --network=none` to isolate a step.
- Steps are cached like Docker's: a step reruns only if it, its inputs (files
  for `COPY`/`ADD`; command, environment and user for `RUN`) or an earlier step
  changed. `--no-cache` skips the cache; `aic prune` clears it.
- Images are kept in `~/.aic/store`. Log in to registries with `aic login`.

## Commands

```
aic build [flags] [CONTEXT]      build CONTEXT/Dockerfile (default context: .)
  -t, --tag NAME[:TAG]           name the image (repeatable); unqualified names go under localhost/
  -f, --file PATH                Dockerfile path
  --build-arg KEY[=VALUE]        set a build argument (repeatable)
  --target STAGE                 build up to this stage
  --platform OS/ARCH[/VARIANT]   target platform (default linux/<host arch>)
  --pull                         always pull base images
  --no-cache                     run every step instead of using the build cache
  -q, --quiet                    print only the image digest

aic run [flags] IMAGE [COMMAND [ARG...]]   run a command in a new container (see below)
aic login -i [REGISTRY]          guided login: pick a registry, get help creating a token
aic login [-u USER] [--password-stdin] REGISTRY   log in (credentials are verified first)
aic logout [-a] [REGISTRY]       remove credentials saved by aic login
aic images [-q] [--no-trunc]     list local images
aic prune [--vm]                 remove the build cache and unpacked images (and the VM's cached files)
aic clean [-f]                   remove all images, caches and VM files (keeps credentials)
aic tag IMAGE TARGET...          add names to a local image
aic rmi [-f] [-a] IMAGE...       remove images (a name untags; an ID removes all its names)
aic pull [--platform OS/ARCH] IMAGE
aic push IMAGE [DESTINATION]     IMAGE may be a name or a digest (prefix)
aic version
```

aic never uses the `latest` tag: a name without a tag (`-t hello`, or a push
destination like `ghcr.io/org/hello`) is tagged `sha256-<image digest>`, and
`:latest` is rejected. A name without a tag also refers to the repository's
newest local image, so after `aic build -t hello`, `aic run hello` and
`aic push hello ghcr.io/org/hello` use what you just built.

Flags may come before or after positional arguments (for `aic run`, only
before IMAGE). Shell completion: `aic completion bash|zsh|fish|powershell`.

## Running containers

`aic run` runs a command in a throwaway container, for example to let an AI
agent or untrusted code work on a project without access to the rest of
your machine:

```sh
aic run alpine:3.20 echo hello
aic run -it python:3.12 python                 # interactive, on a terminal
aic run -w . python:3.12 python -m pytest      # with a copy of the current directory
aic run -w . --copy-back python:3.12 ruff format .   # ...and keep its changes
aic run -w .:/src --network=host golang:1.26 go build ./...
```

```
  -w, --workspace DIR[:PATH]   copy DIR into the container at PATH (default /workspace)
  --copy-back                  copy the container's changes to the workspace back to DIR when it exits
  --network none|host          none (default): loopback only; host: the host's network (on macOS, the VM's)
  -i, --interactive            connect standard input
  -t, --tty                    run on a terminal
  -e, --env KEY[=VALUE]        set a variable; KEY alone passes the host's value (repeatable)
  -u, --user USER[:GROUP]      run as this user (default: the image's)
  --workdir DIR                working directory (default: the workspace, else the image's)
  --platform OS/ARCH           image platform to pull
```

- The container sees no host files and, by default, has no network.
- With `-w`, DIR is copied in before the command starts and owned by the
  command's user; the container works on its copy and DIR is never changed.
- With `--copy-back`, new, changed and deleted files are copied back to DIR
  when the command exits, whatever its exit status. Files you edit on the
  host meanwhile are kept unless the container changed them too. If the
  container removes the workspace directory itself, nothing is copied back.
- The image is pulled if it isn't in the store. The container is removed
  when the command exits.
- aic exits with the command's exit status: 125 if it couldn't run the
  command, 127 if the command wasn't found.

### Example: Claude Code

[examples/claude-code](examples/claude-code/Dockerfile) packages
[Claude Code](https://docs.anthropic.com/en/docs/claude-code). In a container
it can only change the copy of your project, so it is reasonable to let it run
without permission prompts:

```sh
aic build -t claude-code examples/claude-code
export ANTHROPIC_API_KEY=sk-ant-...

# Interactive session on the current directory; its changes come back.
aic run -it -w . --copy-back --network=host -e ANTHROPIC_API_KEY claude-code

# Headless: make a change, then review it with git diff.
aic run -w . --copy-back --network=host -e ANTHROPIC_API_KEY claude-code \
  -p "make the failing tests pass" --dangerously-skip-permissions
git diff

# Read-only: ask about the code; nothing comes back.
aic run -w . --network=host -e ANTHROPIC_API_KEY claude-code -p "explain the build"
```

Claude Code needs the network to reach the API (`--network=host`; on macOS
that is the VM's network, not your Mac's localhost). Its settings and login
are not kept between runs; pass the API key with `-e` each time.

## Go package

Every command is also a method of `aic.Client` in the
[`github.com/ai4-labs/aic`](https://pkg.go.dev/github.com/ai4-labs/aic) package:

```go
func main() {
	aic.Init() // first: containers re-execute the program on Linux

	c, err := aic.New(aic.Options{Progress: os.Stderr})
	...
	img, err := c.Build(ctx, aic.BuildOptions{ContextDir: ".", Tags: []string{"app"}})
	code, err := c.Run(ctx, aic.RunOptions{
		Image:     img.Digest,
		Command:   []string{"make", "test"},
		Workspace: ".",
		CopyBack:  true,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	})
}
```

`Build`, `Run`, `Pull`, `Push`, `Images`, `Tag`, `Remove`, `Prune`, `Clean`,
`Login`, `Logout` and `LoggedInUser` correspond to the commands. Errors caused
by invalid arguments wrap `aic.ErrInvalid`. On macOS, the program needs the
`com.apple.security.virtualization` entitlement to run containers; without it,
the first build or run signs the executable (ad hoc) and restarts it.

## The VM (macOS)

On macOS, each `aic build` that has `RUN` steps, and each `aic run`, boots its
own small Linux VM (about a second) and stops it when done, so concurrent
builds and runs don't wait for each other. The VM runs a
[LinuxKit](https://github.com/linuxkit/linux) kernel and its vsock modules,
downloaded once (a ~200 MB download, pinned and checksummed); the guest agent
is embedded in aic. Images and workspaces reach the VM over vsock. If the VM
fails, the error includes the end of its console log. `aic prune --vm` deletes
the cached kernel, modules and initramfs.

## Dockerfile support

All Dockerfile instructions are supported, including multi-stage builds,
heredocs, `.dockerignore`, and `ADD` of URLs and local archives.

Not supported yet: `RUN --mount`, `ADD` from git, xz archives,
`COPY --parents`/`--exclude`, and `ONBUILD` triggers of base images.

## Building

```sh
make            # builds ./aic
make install    # installs it to $(go env GOPATH)/bin (override with BINDIR=...)
```

On macOS, `make` cross-compiles the Linux guest (`cmd/aic-guest`), embeds it,
and signs `aic` with the `com.apple.security.virtualization` entitlement
(ad hoc). A plain `go build` from a checkout works for everything except
`RUN` and `aic run`; use `make` for those. Requires Go 1.26+ and, on macOS,
the Xcode command line tools (cgo); tested on macOS 15. Make sure you have an
up-to-date Command Line Tools installed (`xcode-select --install`).

| Variable | |
|---|---|
| `AIC_STORE` | image store (default `~/.aic/store`) |
| `AIC_AUTH_FILE` | credentials saved by `aic login` (default `~/.aic/auth.json`, mode 0600; secrets go to a Docker credential helper such as `docker-credential-osxkeychain` if one is installed when the file is created) |
| `AIC_VM_DIR` | VM kernel, initramfs and guest (default `~/.aic/vm`) |
| `AIC_VM_CPUS`, `AIC_VM_MEMORY` | VM size on macOS (default min(4, host CPUs) and 4096 MiB) |
| `SOURCE_DATE_EPOCH` | image creation time |

On Linux, install `newuidmap`/`newgidmap` (shadow-utils) and add an
`/etc/subuid` and `/etc/subgid` entry for your user so that `RUN` can use users
other than root; without them, chowning files to other users fails.

On Ubuntu 23.10 and later, AppArmor blocks unprivileged user namespaces by
default. Allow them with `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`
or an AppArmor profile for aic.

## Limitations

- Each `RUN` and `aic run` unpacks the full image (and workspace) into a
  tmpfs, so memory bounds how large they can be (on macOS, the VM's memory).
- On macOS, every build and run copies the whole image into its VM. For
  `aic run`, the store keeps each image unpacked (in `~/.aic/store/flat`,
  as large as the image's files) so this takes about 1.5 s for the Claude
  Code example; `aic prune` removes these copies.
- The macOS VM is arm64 only (Apple silicon), and `RUN` uses the host's
  architecture: no emulation for other `--platform`s yet.

## License

Apache License 2.0; see [LICENSE](LICENSE).
