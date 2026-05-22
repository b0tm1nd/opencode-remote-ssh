import { readFileSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import type { WorkspaceBinding } from "./types.js";

const STATE_FILE = `${homedir()}/.local/share/opencode/remote-ssh-state.json`;

export class RuntimeState {
  private readonly bindings = new Map<string, WorkspaceBinding>();

  constructor() {
    this.load();
  }

  private stateDir(): string {
    const dir = STATE_FILE.substring(0, STATE_FILE.lastIndexOf("/"));
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true });
    }
    return dir;
  }

  private load(): void {
    try {
      if (existsSync(STATE_FILE)) {
        const data = JSON.parse(readFileSync(STATE_FILE, "utf-8"));
        if (Array.isArray(data)) {
          for (const binding of data) {
            if (binding && binding.workspaceID) {
              this.bindings.set(binding.workspaceID, binding as WorkspaceBinding);
            }
          }
        }
      }
    } catch {
      // Ignore corrupted state file
    }
  }

  private save(): void {
    try {
      this.stateDir();
      writeFileSync(STATE_FILE, JSON.stringify(Array.from(this.bindings.values()), null, 2));
    } catch {
      // Ignore write errors
    }
  }

  set(binding: WorkspaceBinding): void {
    this.bindings.set(binding.workspaceID, binding);
    this.save();
  }

  get(workspaceID: string): WorkspaceBinding | undefined {
    return this.bindings.get(workspaceID);
  }

  delete(workspaceID: string): void {
    this.bindings.delete(workspaceID);
    this.save();
  }

  list(): WorkspaceBinding[] {
    return Array.from(this.bindings.values());
  }
}
