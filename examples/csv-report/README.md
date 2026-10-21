# csv-report

A batch job that reads input from a workspace folder and writes results back.
Without a config the workspace is read-only. The operator's config makes it writable.

```sh
# Deny-all: the summary is printed, but writing out/summary.txt is blocked (exit 1).
aic run --workspace examples/csv-report/data examples/csv-report

# The operator makes the workspace writable.
aic run --config examples/csv-report/config.json \
        --workspace examples/csv-report/data examples/csv-report
cat examples/csv-report/data/out/summary.txt
```

```
east       2 orders     309.00
north      2 orders     195.75
west       3 orders     510.75
wrote out/summary.txt
```

Things to notice:
- [config.json](config.json) is supplied by whoever runs the app. The app can't grant
  itself write access.
- The grant covers the workspace folder only. Your home folder, `/tmp` and the rest of
  the system stay off-limits.
- To narrow the grant to one subfolder, use `"$WORKSPACE/out"` instead.
