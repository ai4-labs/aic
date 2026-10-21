# sandbox

Use aic as a disposable sandbox for code an AI agent just wrote. The agent drops
`solution.sh` into a job folder, and aic runs it with no network, no access to your
files, and a read-only view of the job folder.

```sh
export OPENAI_API_KEY=sk-...       # in your shell, not visible to the job
aic run --workspace examples/sandbox/job examples/sandbox
```

```
== task: primes below 50
2 3 5 7 11 13 17 19 23 29 31 37 41 43 47
== attempts
  blocked  list other users' home folders (/Users)
  blocked  read /etc/passwd
  blocked  reach the network (curl example.com)
  blocked  overwrite its own job file
  blocked  write to /tmp
  blocked  find API keys in the environment
  ALLOWED  write to its scratch folder
```

Things to notice:
- No `--config` means deny-all. The only writable place is the per-run scratch
  folder, which is deleted when the run ends.
- [job/solution.sh](job/solution.sh) is ordinary shell. Edit it to try your own
  escapes.
- To let the job hand back results, make the workspace writable in a config:
  `{"filesystem": {"writable": ["$WORKSPACE"]}}`.
