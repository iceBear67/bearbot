import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, describe, expect, test } from "vitest";
import { readConfig, renderPromptTemplate } from "../src/bridge.ts";
import { Diagnostics, diagnosticsFile, formatDiagnosticsForTurn } from "../src/diagnostics.ts";
import { Memory, shouldSteerMemory } from "../src/memory.ts";
import { rollbackAckAllowed, rollbackReminder, rollbackTurnBody } from "../src/rollback.ts";
import { agentEnv, EXTENSION_DIR, removeTempDirs, tempDir } from "./helpers.ts";

afterAll(removeTempDirs);

describe("memory (AGENTS.md)", () => {
  test("replaces the workspace entry, appends it when missing, removes it without AGENTS.md", () => {
    const workspace = tempDir("bbim-mem-");
    writeFileSync(join(workspace, "AGENTS.md"), `${String.fromCharCode(0xfeff)}# 记忆 v1\n`);
    const memory = new Memory(workspace);
    memory.reload();
    expect(memory.content).toBe("# 记忆 v1\n");

    const withOverride = [
      { path: "/profile/agent/AGENTS.md", content: "profile" },
      { path: join(workspace, "AGENTS.override.md"), content: "override" },
    ];
    memory.applyToContextFiles(withOverride);
    expect(withOverride).toEqual([
      { path: "/profile/agent/AGENTS.md", content: "profile" },
      { path: join(workspace, "AGENTS.md"), content: "# 记忆 v1\n" },
    ]);

    const without = [{ path: "/AGENTS.md", content: "root" }];
    memory.applyToContextFiles(without);
    expect(without.at(-1)).toEqual({ path: join(workspace, "AGENTS.md"), content: "# 记忆 v1\n" });

    writeFileSync(join(workspace, "AGENTS.md"), "# 记忆 v2\n");
    const stillCached = [{ path: join(workspace, "AGENTS.md"), content: "pi" }];
    memory.applyToContextFiles(stillCached);
    expect(stillCached[0].content).toBe("# 记忆 v1\n");
    memory.reload();
    expect(memory.steerText()).toContain("# 记忆 v2");

    const deleted = tempDir("bbim-mem-none-");
    const empty = new Memory(deleted);
    empty.reload();
    const files = [{ path: join(deleted, "CLAUDE.md"), content: "claude" }];
    empty.applyToContextFiles(files);
    expect(files).toEqual([]);
  });

  test("a directory named AGENTS.md is an error", () => {
    const workspace = tempDir("bbim-mem-dir-");
    mkdirSync(join(workspace, "AGENTS.md"));
    expect(() => new Memory(workspace).reload()).toThrow();
  });

  test("memory is steered after a compaction only when the round continues", () => {
    expect(shouldSteerMemory(true, null)).toBe(true);
    expect(shouldSteerMemory(false, "toolUse")).toBe(true);
    expect(shouldSteerMemory(false, "stop")).toBe(false);
    expect(shouldSteerMemory(false, null)).toBe(false);
  });
});

describe("prompt templates", () => {
  const vars = { WORKSPACE: "/w", PROFILE_DIR: "/p", RUN_DIR: "/r", SNAPSHOTS_GIT: "/p/bearbot/snapshots.git", SNAPSHOT_WORKTREE: "/" };

  test("bearbot.md has both classifier variants and no placeholder left", () => {
    const template = readFileSync(join(EXTENSION_DIR, "prompt", "bearbot.md"), "utf8");
    const enabled = renderPromptTemplate(template, vars, true);
    const disabled = renderPromptTemplate(template, vars, false);
    expect(enabled).toContain("member 回合中的每一次工具调用在执行前都要经过 classifier 检查");
    expect(enabled).not.toContain("管理员关闭了 classifier");
    expect(disabled).toContain("只检查对 `CLASSIFIER.md` 的修改");
    expect(disabled).not.toContain("在执行前都要经过 classifier 检查");
    expect(enabled).toContain("git --git-dir=/p/bearbot/snapshots.git --work-tree=/ log");
    expect(enabled).not.toMatch(/\{\{/);
  });

  test("bearbot.md names every profile item the location check rejects", () => {
    const text = renderPromptTemplate(readFileSync(join(EXTENSION_DIR, "prompt", "bearbot.md"), "utf8"), vars, true);
    const sentence = text.split("\n").find((line) => line.startsWith("`/p/agent/` 中的"));
    expect(sentence).toBeDefined();
    // The list of the daemon's location check (DESIGN 9.1, daemon/internal/check).
    for (const item of ["extensions/", "skills/", "prompts/", "mcp.json", "AGENTS.md", "AGENTS.override.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD", "SYSTEM.md", "APPEND_SYSTEM.md"]) {
      expect(sentence).toContain(`\`${item}\``);
    }
    expect(sentence).toContain("agent 进程也不会启动");
  });

  test("classifier.md uses the run and profile directories", () => {
    const text = renderPromptTemplate(readFileSync(join(EXTENSION_DIR, "prompt", "classifier.md"), "utf8"), vars, true);
    expect(text).toContain("访问 `/r/` 下的 socket");
    expect(text).toContain("访问 `/p`");
    expect(text).toContain("decide");
  });

  test("an unknown placeholder is an error", () => {
    expect(() => renderPromptTemplate("{{NOPE}}", vars, true)).toThrow("{{NOPE}}");
  });
});

describe("configuration from the environment", () => {
  test("every variable is read and checked", () => {
    const root = tempDir("bbim-cfg-");
    const config = readConfig(agentEnv(root, root, { BEARBOT_TRIAL: "1" }), EXTENSION_DIR);
    expect(config).toMatchObject({
      workspaceDir: root,
      workspaceReal: root,
      classifierModel: { provider: "anthropic", modelId: "claude-haiku-4-5-20251001" },
      classifierTimeoutMs: 15000,
      rulesMaxBytes: 32768,
      observeLimit: 30,
      queuedSummaryAfter: 10,
      holdWarnAfterMs: 600000,
      trial: true,
    });
    expect(() => readConfig(agentEnv(root, root, { BEARBOT_OBSERVE_LIMIT: "30s" }), EXTENSION_DIR)).toThrow("BEARBOT_OBSERVE_LIMIT");
    expect(() => readConfig(agentEnv(root, root, { BEARBOT_QUEUED_SUMMARY_AFTER: "0" }), EXTENSION_DIR)).toThrow("BEARBOT_QUEUED_SUMMARY_AFTER 不能小于 1");
    expect(() => readConfig(agentEnv(root, root, { BEARBOT_CLASSIFIER_MODEL: "haiku" }), EXTENSION_DIR)).toThrow("<provider>/<model>");
    expect(() => readConfig(agentEnv(root, root, { BEARBOT_TRIAL: "yes" }), EXTENSION_DIR)).toThrow("BEARBOT_TRIAL");
    expect(() => readConfig(agentEnv(root, join(root, "missing")), EXTENSION_DIR)).toThrow();
  });
});

describe("diagnostics file", () => {
  test("records persist across processes until delivered", () => {
    const workspace = tempDir("bbim-diag-");
    const file = diagnosticsFile(workspace);
    const first = new Diagnostics(file);
    const a = first.add({ source: "turn", level: "error", message: "回合出错", detail: "<boom>" });
    first.add({ source: "input", level: "warning", message: "拒绝了输入" });
    const second = new Diagnostics(file);
    expect(second.pending().map((record) => record.source)).toEqual(["turn", "input"]);
    expect(formatDiagnosticsForTurn(second.pending())).toContain("&lt;boom&gt;");
    second.markDelivered([a.id]);
    const third = new Diagnostics(file);
    expect(third.pending().map((record) => record.source)).toEqual(["input"]);
    expect(third.tail(10).map((record) => record.delivered)).toEqual([true, false]);
  });
});

describe("rollback", () => {
  const record = {
    id: 3,
    target: "apply/12^",
    saved: "rollback/3/before",
    reverted: [{ ref: "apply/12", summary: "加入防抖" }],
    requesters: ["telegram:1"],
    thread: "telegram:-100123",
    notified: false,
  };

  test("turn body and reminder name the facts of the rollback", () => {
    const body = rollbackTurnBody(record).join("\n");
    expect(body).toContain("管理员 telegram:1 把 bot 回滚到了快照 apply/12^，回滚之前的状态保存在 rollback/3/before。");
    expect(body).toContain("被撤销的应用：apply/12：加入防抖");
    expect(rollbackReminder({ ...record, requesters: null })).toContain("管理员 （通过命令行）");
  });

  test("ack only after a normal admin turn from IM admins", () => {
    const normal = { role: "admin" as const, viaPrompt: false, allSendersAdmins: true, messageCount: 1, aborted: false, error: undefined };
    expect(rollbackAckAllowed(normal)).toBe(true);
    expect(rollbackAckAllowed({ ...normal, viaPrompt: true, messageCount: 0 })).toBe(false);
    expect(rollbackAckAllowed({ ...normal, role: "member" })).toBe(false);
    expect(rollbackAckAllowed({ ...normal, role: "system", messageCount: 0 })).toBe(false);
    expect(rollbackAckAllowed({ ...normal, aborted: true })).toBe(false);
    expect(rollbackAckAllowed({ ...normal, error: "429" })).toBe(false);
  });
});
