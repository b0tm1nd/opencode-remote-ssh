# Workspace Creation Fails Despite Successful Bootstrap

## Environment
- **OpenCode version**: 1.15.7 (also tested 1.14.51)
- **Plugin**: opencode-remote-ssh (custom SSH workspace provider)
- **SDK version**: @opencode-ai/plugin@1.14.39 (plugin built against), 1.15.7 (TUI)
- **OS**: Linux
- **Mode**: TUI (CLI) and Desktop AppImage

## Steps to Reproduce
1. Configure plugin in `opencode.jsonc`:
   ```json
   {
     "plugin": [
       "opencode-chrome-devtools",
       ["/path/to/plugin", {
         "providers": {
           "default": {
             "hosts": [{
               "name": "contabo",
               "ssh": { "host": "...", "user": "root", "port": 22, "identityFile": "~/.ssh/id_ed25519" }
             }]
           }
         }
       }]
     ]
   }
   ```
2. Run `OPENCODE_EXPERIMENTAL_WORKSPACES=1 opencode`
3. Click "Create Workspace" → "SSH Provider"
4. Observe ~10 second wait, then "Creating Workspace Failed"

## Expected Behavior
Workspace should be created and appear in the workspace list.

## Actual Behavior
- Plugin's `create` method returns successfully (~7.5 seconds)
- `state.set` completes
- `getTarget` is called and returns valid binding (localPort, token)
- Health endpoint returns HTTP 200
- Workspace IS saved to `opencode.db` (SQLite)
- Plugin state IS persisted to disk (JSON file)
- **TUI still shows "Creating Workspace Failed"**
- Workspace does NOT appear in workspace list

## Root Cause Analysis
The workspace adapter's `create`, `target`, and all internal operations succeed. The `create` function returns `Promise<void>` without error. Yet the TUI displays an error. This indicates a bug in the TUI's workspace creation handler — likely in the server-side handler for `POST /experimental/workspace`.

### Additional Findings
1. **No npm package**: `npm view opencode` returns 404. Actual package name is `opencode-ai`.
2. **15-char process name truncation**: `pgrep opencode-remote-stub` fails because Linux truncates process names to 15 chars. `pkill -f` needed but risks killing the SSH shell.
3. **Stale SSH tunnels**: Multiple leftover `ssh -f -N -L` processes accumulate across restarts.
4. **`syncList` API exists but can't be triggered**: `POST /experimental/workspace/sync-list` could register workspaces but is not called automatically.

## Workaround
The `remote-workspace-create` tool (provided by plugin) successfully creates the workspace. The workspace tunnel and stub function correctly (health check passes). However, the TUI workspace list does not reflect this state, making the workspace unusable through the TUI interface.
