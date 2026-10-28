import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { StopReason } from "@earendil-works/pi-ai";
import { escapeAttr } from "./render.ts";

/**
 * Memory is the single file `<workspace>/AGENTS.md` (DESIGN 7.5). The cache is refreshed on session_start and
 * session_compact and put into the system prompt in before_agent_start, so a change made in the middle of a session
 * reaches the prompt after the next new session or compaction.
 */
export class Memory {
  private cached: string | null = null;
  readonly file: string;

  constructor(private readonly workspaceReal: string) {
    this.file = join(workspaceReal, "AGENTS.md");
  }

  get content(): string | null {
    return this.cached;
  }

  reload(): void {
    try {
      this.cached = stripBom(readFileSync(this.file, "utf8"));
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      this.cached = null;
    }
  }

  /**
   * pi loads one context file per directory (AGENTS.override.md > AGENTS.md > AGENTS.MD > CLAUDE.md > CLAUDE.MD).
   * The entry of the workspace directory is replaced by the cached AGENTS.md, or appended when pi loaded none;
   * without AGENTS.md the workspace entry is removed, so only AGENTS.md serves as memory.
   */
  applyToContextFiles(contextFiles: { path: string; content: string }[]): void {
    const index = contextFiles.findIndex((entry) => dirname(entry.path) === this.workspaceReal);
    if (this.cached === null) {
      if (index >= 0) contextFiles.splice(index, 1);
      return;
    }
    const entry = { path: this.file, content: this.cached };
    if (index >= 0) contextFiles[index] = entry;
    else contextFiles.push(entry);
  }

  /** Content of the `bearbot-memory` steer message sent after a compaction in the middle of a round. */
  steerText(): string {
    const body = this.cached ?? "（AGENTS.md 不存在，当前没有记忆）";
    return `<bearbot-memory path="${escapeAttr(this.file)}">\n上下文刚刚被压缩，以下是工作区 AGENTS.md 的最新内容（长期记忆）：\n\n${body}\n</bearbot-memory>`;
  }
}

const BOM = 0xfeff;

function stripBom(text: string): string {
  return text.charCodeAt(0) === BOM ? text.slice(1) : text;
}

/**
 * Whether a compaction during a turn should steer the refreshed memory into the round (contracts 9.1 item 6): only
 * when the round continues after the compaction, i.e. the compaction retries the request or the last assistant
 * message asked for tool calls. Otherwise the next round's before_agent_start uses the refreshed cache.
 */
export function shouldSteerMemory(willRetry: boolean, lastAssistantStopReason: StopReason | null): boolean {
  return willRetry || lastAssistantStopReason === "toolUse";
}
