# Bugs in bootstrap and workspace integration

Found and fixed several issues while setting up and testing this plugin with OpenCode TUI (v1.15.7).

## 1. SCP fails when stub binary is still running

**Problem:** During bootstrap, the plugin SCPs the stub binary to the remote host. If the stub is already running, the file is locked and SCP fails (`text file busy`).

**Fix:** Kill the stub process before SCP. The current bootstrap sequence needs `pkill` before `scp`.

## 2. `pkill -f` kills the SSH shell itself

**Problem:** Using `pkill -f opencode-remote-stub` via SSH kills not just the stub but also the parent SSH session, because the SSH command line itself contains `opencode-remote-stub`.

**Fix:** Use `fuser -k <port>/tcp` to kill by port instead, or use a more specific pattern.

## 3. `pkill` without `-f` doesn't match (15-char process name limit)

**Problem:** Linux truncates process names to 15 characters. `opencode-remote-stub` is 20 chars, so `pkill opencode-remote-stub` silently fails to find the process.

**Fix:** Same as #2 — kill by port with `fuser -k <port>/tcp`.

## 4. Stale SSH tunnels accumulate

**Problem:** Every workspace creation leaves a `ssh -f -N -L ...` process. On retries, these pile up. Over time you get many duplicate tunnels.

**Fix:** Track or kill previous tunnel processes before creating new ones.

## 5. State not persisted across restarts

**Problem:** After OpenCode TUI restarts, `getTarget` is called but no state is available — the tunnel localPort and token are lost. This causes workspace creation to fail silently.

**Fix:** Persist state (localPort, token) to a JSON file on disk so `getTarget` can recover after restart.

## 6. `resolveProvider` fails when `extra` is empty

**Problem:** When no `extra` field is provided in workspace creation payload, `resolveProvider` returns `null` instead of using the first available provider.

**Fix:** Fall back to `Object.keys(providers)[0]` when `extra` is empty/null.

## 7. Bootstrap is slow (~12s) due to sequential SSH commands

**Problem:** Each mkdir, pkill, SCP (token), and stub start is a separate SSH connection. This makes bootstrap take ~12 seconds.

**Fix:** Merge `mkdir + pkill + printf(token)` into one SSH session, replace token SCP with `printf`. Reduces bootstrap to ~7.5s.

---

All fixes are implemented in our local copy. Happy to submit a PR if interested.
