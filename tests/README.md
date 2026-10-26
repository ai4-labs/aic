# Docker compliance tests

These tests build the same Dockerfiles with `docker build` and `aic build`,
run the same commands in the resulting images with `docker run` and `aic run`,
and compare:

- the **image config** (`Env`, `Cmd`, `Entrypoint`, `WorkingDir`, `User`,
  `Labels`, `ExposedPorts`, `Volumes`, `StopSignal`, `Healthcheck`, `Shell`,
  `OnBuild`), read from `docker image inspect` and from aic's OCI store;
- the **output and exit status** of each command (stderr only where a case
  asks for it); file-system cases print each path's mode, owner, type, link
  target and content hash so that layer contents are compared exactly.

The cases are in [`cases_test.go`](cases_test.go), the harness in
[`compliance_test.go`](compliance_test.go). Each case is a subtest of
`TestCompliance`; a failure prints what docker produced, what aic produced and
the difference.

## Running

```sh
make                                   # builds ./aic (signed, with the VM guest)
go test -tags compliance ./tests/ -v   # needs a running Docker daemon
```

The build tag keeps them out of `go test ./...`. `AIC=/path/to/aic` picks a
binary (default `../aic`, then `$PATH`). aic uses a temporary `AIC_STORE`;
docker images are tagged `aic-compliance-<pid>-*` and removed at the end.
Run one case with `-run 'TestCompliance/env$'`. Some cases need the network
(`add-url`, `base-image-config`, and the base image pulls).

## Results

Last run: 2026-10-25, macOS 26.6.2 (Apple silicon), Docker Desktop 29.4.0
(BuildKit), aic at commit f1af380. 45 cases, about 2.5 minutes.

### Passing

| Area | Cases | What is compared |
|---|---|---|
| `COPY` | `copy-basics`, `copy-chown-chmod`, `copy-into-workdir`, `copy-symlink-context` | files, dirs, globs, trailing-slash rules, nested dest creation, `--chown` by uid and name, `--chmod`, relative dest under `WORKDIR`, env expansion in paths, symlinks in the context copied as symlinks |
| `ADD` | `add-archive`, `add-url` | local tar.gz extracted (or kept as a file when the dest is a file name), `--chown`, URL downloads with docker's 0600 mode |
| `RUN` | `run-forms`, `run-changes-layers`, `run-network-none` | shell and exec forms, env expansion only in shell form, line continuation, deletions and dir-to-file replacements across layers (whiteouts, including base-image files), setuid and sticky bits, symlinks, fifos, `--network=none` has no network while a plain `RUN` has |
| `ENV` / `ARG` | `env` (runs), `env-at-run`, `arg`, `arg-override`, `arg-predefined` | `KEY=VALUE`, quoted and legacy forms, `${VAR:-default}`, `${VAR:+alt}`, escaped `\$`, `-e KEY=VALUE` and `-e KEY` from the host at run time, `HOME` for `-u name` and `-u uid`, `ARG` before `FROM`, redeclared args, args absent from the final env, `--build-arg`, `TARGETOS`/`TARGETARCH`/`TARGETPLATFORM`/`BUILDPLATFORM` |
| `WORKDIR` / `USER` | `workdir`, `run-as-user` (config) | relative, absolute, env-expanded and `..` paths, directories created, `--workdir` override including a new dir, `USER` by name, uid, `uid:gid` and `name:group` |
| `CMD` / `ENTRYPOINT` / `SHELL` | `cmd-entrypoint`, `cmd-shell-form`, `entrypoint-shell-form`, `entrypoint-resets-cmd`, `cmd-inherited`, `shell` | exec and shell forms, `CMD` as default args, override by `run` args, shell-form `ENTRYPOINT` ignoring `CMD`, `CMD` cleared when `ENTRYPOINT` is set, inherited `CMD`, custom `SHELL` used by `RUN`, `CMD` and `ENTRYPOINT` |
| Metadata | `metadata`, `healthcheck-forms`, `base-image-config` | `LABEL` forms, `EXPOSE` with protocols and ranges, `VOLUME` forms, `STOPSIGNAL`, `HEALTHCHECK` with options, `CMD` exec form and `NONE`, `ONBUILD` recorded, `MAINTAINER` accepted; all config inherited from `nginx:alpine` |
| Stages | `multistage`, `multistage-inherits-nothing`, `target` | `COPY --from` by name, index and external image, with `--chown`, stage config not leaking into the next, `--target` |
| Parsing | `dockerignore`, `heredoc`, `syntax`, `escape-directive` | `.dockerignore` with negation, `**` and directories; `RUN` and `COPY` heredocs including `<<-`, two heredocs on one `RUN`, `--chmod` with a heredoc; comments inside continuations, lowercase instructions, trailing spaces; `# escape=`` ` |
| Running | `run-stdin`, `run-exit-status` (exit 3, not found, stdout and stderr) | `-i` stdin, exit status passed through, 127 for a missing command |
| Errors | `bad-dockerfile`, `bad-instruction`, `run-fails`, `copy-outside-context` | both builds fail |

### Differences and missing features

Each item names the subtest that fails and what aic does instead of docker.

**Dockerfile features aic rejects** (the build fails):

- `run-mount-cache`: `RUN --mount=...` is not supported.
- `copy-parents`: `COPY --parents` is not supported.
- `copy-exclude`: `COPY --exclude` is not supported.
- `stopsignal-number`: `HEALTHCHECK --start-interval` is not supported.
  (`STOPSIGNAL 9` by number is not reached in that case; retest after the fix.)

**Image config:**

- `env/config`: redefining a variable (`ENV PATH=...` on an image that has
  `PATH`) keeps its position in docker's `Env` list; aic moves it to the end.
  The values are the same.
- `from-scratch/config`: docker gives images built `FROM scratch` a default
  `PATH` in the config; aic records no `Env`. At run time aic does provide
  the same default `PATH` (the `from-scratch` runs pass).

**Process identity:**

- `run-as-user/run0`, `run-as-user/run1`, `run-process/run0`: docker adds the
  primary group to the supplementary groups (`id` prints `groups=1500(app)`,
  and for root `groups=0(root),0(root),1(bin),...`); aic sets only the groups
  listed in `/etc/group`. Build-time `RUN` as a user and `aic run` both differ.
- `run-process/run4`, `run-exit-status/run3`: in docker the command is PID 1;
  in aic it runs under `aic-container-init`. So `kill -9 $$` from the shell
  is ignored in docker (PID 1 ignores signals, exit 0) and kills it in aic
  (exit 137). This matches `docker run --init`.
- `run-exit-status/run2`: running a file without execute permission exits 126
  in docker and 125 in aic.

**Container environment** (runtime-provided, expected to differ):

- `run-process/run5`: `/dev` has no `mqueue` and `core` in aic.
- `run-filesystem-view/run1`: with the network off, docker still writes a
  `nameserver` into `/etc/resolv.conf`; aic's has none.
- `run-filesystem-view/run2`: the root is a tmpfs in aic (overlay in docker);
  aic does not mount cgroup2 at `/sys/fs/cgroup`, bind-mount `/etc/hosts`,
  `/etc/hostname` and `/etc/resolv.conf`, or mask `/proc/kcore`, `/proc/keys`,
  `/proc/sys` and the other paths docker hides.

### Not covered

- `ADD` from git, xz archives, `ONBUILD` triggers of a base image (documented
  as unsupported; `ONBUILD` recording itself is tested in `metadata`).
- Build cache behaviour, `--platform` emulation, `push`/`pull`, workspaces
  (`-w`, `--copy-back`) and `--network=host`: no docker equivalent to diff
  against, or covered by the unit tests.
- File timestamps and layer digests: docker and aic do not produce identical
  layers, only identical contents.
