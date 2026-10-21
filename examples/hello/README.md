# hello

The smallest aic project: one shell script, no toolchain, no config.

```sh
aic run examples/hello
```

```
Hello from aic!
  working dir: /private/var/folders/.../aic-run-123/app
  HOME:        /private/var/folders/.../aic-run-123/scratch/home
  PATH:        /usr/bin:/bin:/usr/sbin:/sbin
  files:       hello
```

Things to notice:
- The process runs in `app/`, a fresh copy of the files matched by `include`.
- `HOME` and `TMPDIR` point into a per-run scratch folder that is deleted afterwards.
- `GREETING` comes from `entrypoint.env` in [aic.yaml](aic.yaml). Nothing is inherited
  from your shell.

Any command can replace the entrypoint after `--`, and it gets the same sandbox:

```sh
aic run examples/hello -- /usr/bin/uname -a
aic run examples/hello -- /bin/ls /Users        # Operation not permitted
```
