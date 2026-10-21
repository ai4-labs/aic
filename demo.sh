#!/usr/bin/env bash
# demo.sh: walks through how `aic build` keeps aic.lock up to date.
#
# Builds aic from this checkout into a temporary directory, creates a
# throwaway project, and runs each scenario. Nothing outside the temporary
# directory is touched; it is removed on exit.
#
# Usage: ./demo.sh            (set DEMO_PAUSE=1 to pause between steps)

set -uo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

bold=$'\033[1m'; dim=$'\033[2m'; reset=$'\033[0m'
[[ -t 1 ]] || { bold=""; dim=""; reset=""; }

step() {
  echo
  echo "${bold}== $*${reset}"
  if [[ "${DEMO_PAUSE:-0}" == 1 ]]; then read -r -p "${dim}(press enter)${reset}" _; fi
}

# run prints a command as if typed, runs it in the project dir, and shows
# the exit code. Non-zero codes are expected in some steps, so errors do
# not abort the demo.
run() {
  echo "${dim}\$${reset} $*"
  (cd "$work/demo" && "$@")
  echo "${dim}[exit $?]${reset}"
}

echo "${bold}Building aic...${reset}"
mkdir -p "$work/bin"
(cd "$repo" && go build -o "$work/bin/aic" ./cmd/aic) || exit 1
export PATH="$work/bin:$PATH"

(cd "$work" && aic init demo >/dev/null) || exit 1

step "1. A new project has an aic.yaml but no aic.lock"
run ls

step "2. aic build creates aic.lock (exit 3: assembling the bundle comes in M1)"
run aic build
run cat aic.lock

step "3. Building again with nothing changed leaves the lock alone"
run aic build

step "4. Edit aic.yaml, then build with --locked, as CI would: it fails and leaves the lock alone"
echo "# a change" >> "$work/demo/aic.yaml"
run aic build --locked

step "5. A plain aic build notices the change and refreshes the lock"
run aic build
run cat aic.lock

step "6. A lock it can't read (e.g. merge conflict) is never overwritten"
cp "$work/demo/aic.lock" "$work/aic.lock.good"
printf '<<<<<<< HEAD\n{}\n=======\n{}\n>>>>>>> other-branch\n' > "$work/demo/aic.lock"
run aic build
run cat aic.lock
cp "$work/aic.lock.good" "$work/demo/aic.lock"

step "7. To re-resolve everything, delete aic.lock and build"
run rm aic.lock
run aic build

if [[ "$(uname -s)" != Darwin ]]; then
  echo
  echo "${bold}Skipping the run demo: aic run currently needs macOS (Linux sandbox is milestone M3).${reset}"
  exit 0
fi

echo
echo "${bold}== Part 2: running an app in the sandbox${reset}"

# A small app that tries a few things and reports what happened.
mkdir -p "$work/demo/src" "$work/ws/out"
echo "$HOME" > "$work/demo/src/host_home"
echo "quarterly numbers" > "$work/ws/input.txt"
cat > "$work/demo/src/start" <<'EOF'
#!/bin/sh
echo "hello from inside the sandbox (cwd: $(basename "$PWD"))"
echo "host env DEMO_API_KEY: ${DEMO_API_KEY:-<not visible>}"
echo "read workspace/input.txt: $(cat "$AIC_WORKSPACE/input.txt")"
home=$(cat ./src/host_home)
if ls "$home" >/dev/null 2>&1; then echo "!! listed your home directory"; else echo "blocked: listing $home"; fi
if curl -s -m 3 https://example.com -o /dev/null 2>/dev/null; then echo "!! reached the network"; else echo "blocked: network access"; fi
if { echo done > "$AIC_WORKSPACE/out/result.txt"; } 2>/dev/null; then echo "wrote workspace/out/result.txt"; else echo "blocked: writing workspace/out"; fi
echo scratch > "$AIC_SCRATCH/notes.txt" && echo "wrote scratch/notes.txt (deleted after the run)"
EOF
chmod +x "$work/demo/src/start"
export DEMO_API_KEY="sk-not-for-the-sandbox"

step "8. The app (src/start) tries to read, list your home, reach the network, and write"
run cat src/start

step "9. Run with no --config: deny-all. Only reading the workspace is allowed"
run aic run --workspace ../ws

step "10. The operator grants writes to workspace/out with a config file"
cat > "$work/config.json" <<'EOF'
{ "filesystem": { "writable": ["$WORKSPACE/out"] } }
EOF
run cat ../config.json
run aic run --config ../config.json --workspace ../ws
run cat ../ws/out/result.txt

step "11. A config the macOS sandbox can't enforce yet is refused, never silently ignored (exit 125)"
cat > "$work/egress.json" <<'EOF'
{ "network": { "egress": ["api.openai.com:443"] } }
EOF
run aic run --config ../egress.json

step "12. Any command can replace the entrypoint after --; it gets the same sandbox"
run aic run -- /bin/sh -c 'echo "running $0 in $AIC_ROOT"; cat /etc/hosts'

echo
echo "${bold}Done.${reset}"
