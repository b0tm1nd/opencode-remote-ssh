import type { PluginInput, WorkspaceAdapter, WorkspaceInfo, WorkspaceTarget } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { appendFileSync } from "node:fs";
import { request as httpRequest } from "node:http";
import { resolveConfig, type ResolvedPluginConfig } from "./config.js";
import { LeaseManager } from "./leases.js";
import { ProviderRegistry } from "./provider.js";
import { SSHManager } from "./ssh.js";
import { RuntimeState } from "./state.js";

const leases = new LeaseManager();
const state = new RuntimeState();
let config: ResolvedPluginConfig;
let sshManager: SSHManager;
let providers: ProviderRegistry;

function resolveProvider(workspace: WorkspaceInfo) {
  if (workspace.extra && typeof (workspace.extra as Record<string, unknown>).provider === "string") {
    return providers.resolve({
      provider: (workspace.extra as Record<string, unknown>).provider as string,
      host: typeof (workspace.extra as Record<string, unknown>).host === "string"
        ? ((workspace.extra as Record<string, unknown>).host as string)
        : undefined,
      labels: Array.isArray((workspace.extra as Record<string, unknown>).labels)
        ? ((workspace.extra as Record<string, unknown>).labels as unknown[]).filter(
            (value): value is string => typeof value === "string",
          )
        : undefined,
    });
  }

  const firstProvider = Object.keys(config.providers)[0];
  if (!firstProvider) {
    throw new Error("No providers configured for opencode-remote-provider");
  }

  return providers.resolve({ provider: firstProvider });
}

function configureWorkspace(workspace: WorkspaceInfo): WorkspaceInfo {
  const selection = resolveProvider(workspace);

  return {
    ...workspace,
    type: workspace.type || "ssh-provider",
    name: workspace.name ?? selection.host.name,
    extra: {
      ...(workspace.extra ?? {}),
      provider: selection.provider,
      host: selection.host.name,
    },
  };
}

async function registerWithStub(workspace: WorkspaceInfo, localPort: number, token: string): Promise<void> {
  const body = JSON.stringify({
    id: workspace.id,
    type: workspace.type || "ssh-provider",
    name: workspace.name || workspace.id,
    projectID: workspace.projectID,
    status: "ready",
    extra: workspace.extra || { provider: "default" },
  });
  return new Promise((resolve, reject) => {
    const req = httpRequest(
      {
        hostname: "127.0.0.1",
        port: localPort,
        path: "/experimental/workspace",
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
          "Content-Length": Buffer.byteLength(body),
        },
      },
      (res) => {
        let data = "";
        res.on("data", (chunk) => { data += chunk; });
        res.on("end", () => {
          if (res.statusCode === 200) resolve();
          else reject(new Error(`stub register workspace failed: ${res.statusCode} ${data}`));
        });
      },
    );
    req.on("error", reject);
    req.write(body);
    req.end();
  });
}

async function createWorkspace(workspace: WorkspaceInfo): Promise<void> {
  const fs = await import("node:fs");
  const log = (msg: string, data: string) => {
    try {
      const line = `${new Date().toISOString()} [createWorkspace] ${msg}: ${data}\n`;
      fs.appendFileSync("/tmp/opencode-plugin-error.log", line);
      process.stderr.write(line);
    } catch {}
  };

  log("workspace received", JSON.stringify(workspace));

  let selection;
  try {
    selection = resolveProvider(workspace);
    log("selection", JSON.stringify(selection));
  } catch (err) {
    log("resolveProvider failed", err instanceof Error ? err.message : String(err));
    throw err;
  }

  try {
    leases.acquire(selection.host.name, workspace.id, config.defaults.leaseMode);
    log("leases.acquired", "ok");
  } catch (err) {
    log("leases.acquire failed", err instanceof Error ? err.message : String(err));
    throw err;
  }

  try {
    log("before bootstrap", "ok");
    const bootstrap = await sshManager.bootstrap(workspace.id, selection);
    log("after bootstrap success", JSON.stringify({ remotePort: bootstrap.remotePort, localPort: bootstrap.localPort }));

    state.set({
      workspaceID: workspace.id,
      provider: selection.provider,
      host: selection.host.name,
      remotePort: bootstrap.remotePort,
      localPort: bootstrap.localPort,
      token: bootstrap.token,
      leaseMode: config.defaults.leaseMode,
      status: "ready",
    });
    log("state set done", "ok");

    await registerWithStub(workspace, bootstrap.localPort, bootstrap.token);
    log("stub register done", "ok");
  } catch (error) {
    log("bootstrap failed", error instanceof Error ? error.message : String(error));
    leases.release(selection.host.name, workspace.id);
    throw error;
  }
}

async function removeWorkspace(workspace: WorkspaceInfo): Promise<void> {
  const binding = state.get(workspace.id);
  if (!binding) {
    return;
  }

  await sshManager.teardown(binding);
  leases.release(binding.host, workspace.id);
  state.delete(workspace.id);
}

function getTarget(workspace: WorkspaceInfo): WorkspaceTarget {
  try {
    const binding = state.get(workspace.id);
    if (!binding) {
      appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [getTarget] NOT FOUND for ${workspace.id}\n`);
      throw new Error(`Workspace '${workspace.id}' is not active`);
    }
    appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [getTarget] found binding localPort=${binding.localPort}\n`);
    return {
      type: "remote",
      url: `http://127.0.0.1:${binding.localPort}`,
      headers: {
        Authorization: `Bearer ${binding.token}`,
      },
    };
  } catch (err) {
    try {
      appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [getTarget] ERROR: ${err instanceof Error ? err.message : String(err)}\n`);
    } catch {}
    throw err;
  }
}

const sshProviderAdaptor: WorkspaceAdapter = {
  name: "SSH Provider",
  description: "Remote Linux host over SSH-backed Go stub",
  configure: configureWorkspace,
  create: createWorkspace,
  remove: removeWorkspace,
  target: getTarget,
};

export default async function OpencodeRemotePlugin(input: PluginInput, options?: Record<string, unknown>) {
  const fs = await import("node:fs");
  try {
    fs.appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [init] plugin loading, experimental_workspace=${!!input.experimental_workspace}, options=${JSON.stringify(options)}\n`);
  } catch {}

  if (!input.experimental_workspace) {
    throw new Error("[opencode-remote] experimental_workspace not available");
  }

  config = resolveConfig((options as ResolvedPluginConfig | undefined) ?? { providers: {} });
  sshManager = new SSHManager(config);
  providers = new ProviderRegistry(config, leases);

  try {
    fs.appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [init] providers resolved: ${Object.keys(config.providers).length}\n`);
  } catch {}

  input.experimental_workspace.register("ssh-provider", sshProviderAdaptor);

  try {
    fs.appendFileSync("/tmp/opencode-plugin-error.log", `${new Date().toISOString()} [init] registered ssh-provider adaptor\n`);
  } catch {}

  return {
    tool: {
      "remote-workspace-create": tool({
        description: "Create a remote SSH workspace on a configured host",
        args: {
          workspaceName: tool.schema.string().describe("Name for the workspace"),
          provider: tool.schema.string().optional().describe("Provider name from plugin config"),
          host: tool.schema.string().optional().describe("Specific configured host name to use"),
        },
        async execute(args) {
          try {
            const providerName = args.provider || Object.keys(config.providers)[0];
            if (!providerName) {
              throw new Error("No providers configured for opencode-remote-provider");
            }

            const workspaceID = `remote-${Date.now()}-${args.workspaceName.replace(/\s+/g, "-")}`;
            const selection = providers.resolve({
              provider: providerName,
              host: args.host,
            });

            leases.acquire(selection.host.name, workspaceID, config.defaults.leaseMode);

            try {
              const bootstrap = await sshManager.bootstrap(workspaceID, selection);
              state.set({
                workspaceID,
                provider: selection.provider,
                host: selection.host.name,
                remotePort: bootstrap.remotePort,
                localPort: bootstrap.localPort,
                token: bootstrap.token,
                leaseMode: config.defaults.leaseMode,
                status: "ready",
              });
              await registerWithStub(
                { id: workspaceID, type: "ssh-provider", name: args.workspaceName, projectID: "" } as WorkspaceInfo,
                bootstrap.localPort,
                bootstrap.token,
              );
            } catch (error) {
              leases.release(selection.host.name, workspaceID);
              throw error;
            }

            return JSON.stringify({
              success: true,
              workspaceID,
              provider: selection.provider,
              host: selection.host.name,
              message: `Remote workspace '${args.workspaceName}' created on ${selection.host.name}`,
            });
          } catch (error) {
            return JSON.stringify({
              success: false,
              error: error instanceof Error ? error.message : String(error),
            });
          }
        },
      }),
      "remote-workspace-list": tool({
        description: "List active remote workspaces",
        args: {},
        async execute() {
          return JSON.stringify({ workspaces: state.list() });
        },
      }),
      "remote-workspace-remove": tool({
        description: "Remove a remote workspace",
        args: {
          workspaceID: tool.schema.string().describe("Workspace ID to remove"),
        },
        async execute(args) {
          const binding = state.get(args.workspaceID);
          if (!binding) {
            return JSON.stringify({ success: false, error: "Workspace not found" });
          }

          await sshManager.teardown(binding);
          leases.release(binding.host, args.workspaceID);
          state.delete(args.workspaceID);
          return JSON.stringify({ success: true, message: `Workspace ${args.workspaceID} removed` });
        },
      }),
    },
  };
}

export type { PluginConfig } from "./types.js";
