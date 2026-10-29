import { readFileSync, renameSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, describe, expect, test } from "vitest";
import { buildDaemon, callAgent, Daemon, ERROR_NOTICE, TestRoot, waitFor } from "./daemon-env.ts";
import { removeTempDirs, SDK_FILE, tempDir } from "./helpers.ts";

// Messages through the real gateway, bearbot-im and pi, run by the real bearbotd (DESIGN 16 控制台, first item).
// Without credentials pi rejects every dispatched prompt before agent_start ("No API key found for ..."), which
// bearbot-im turns into an error turn (contracts 9.1 item 4).

afterAll(removeTempDirs);

let bin: string;

beforeAll(async () => {
  bin = await buildDaemon(tempDir("bbim-bin-"));
}, 600_000);

const roots: TestRoot[] = [];
const daemons: Daemon[] = [];

afterEach(async () => {
  for (const daemon of daemons.splice(0)) await daemon.stop();
  for (const root of roots.splice(0)) root.remove();
});

async function start(env: TestRoot): Promise<Daemon> {
  const daemon = await Daemon.start(bin, env);
  daemons.push(daemon);
  return daemon;
}

function newRoot(options: { queuedSummaryAfter?: number } = {}): TestRoot {
  const env = new TestRoot(options);
  roots.push(env);
  return env;
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** The first line of the error reply that admin and system turns get (DESIGN 7.2 回合出错, contracts 9.4). */
const FULL_ERROR = "处理时出错：pi 拒绝了这一轮：No API key found for";

/** Records every input text that passed bearbot-im's input check, so the test can read the dispatched turn texts. */
const INPUTS = `import { appendFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("input", (event, ctx) => {
    appendFileSync(join(ctx.cwd, "inputs.jsonl"), \`\${JSON.stringify(event.text)}\\n\`);
    return { action: "continue" };
  });
}
`;

function inputs(env: TestRoot): string[] {
  return env.readLog(join(env.workspace, "inputs.jsonl")).split("\n").filter((line) => line !== "").map((line) => JSON.parse(line) as string);
}

// A console line "as <name>: <text>" becomes a message of the admin test:<name> (middleware may change the sender,
// DESIGN 8.3), so one console makes several senders. Every message the middleware sees is recorded with its seqs.
const SENDERS = `import { appendFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-senders" });
  bearbot.use((msg, next) => {
    const match = /^as (\\w+): ([\\s\\S]*)$/.exec(msg.text);
    if (match) {
      msg.sender = { id: \`test:\${match[1]}\`, name: match[1], role: "admin", isBot: false };
      msg.text = match[2];
    }
    appendFileSync(join(bearbot.workspace.dir, "received.jsonl"), \`\${JSON.stringify({ seq: msg.seq, sender: msg.sender.id, text: msg.text })}\\n\`);
    next(msg);
  }, { order: -100 });
}
`;

type Received = { seq: number[]; sender: string; text: string };

function received(env: TestRoot): Received[] {
  return env.readLog(join(env.workspace, "received.jsonl")).split("\n").filter((line) => line !== "").map((line) => JSON.parse(line) as Received);
}

/**
 * A model whose endpoint refuses connections, with retries: a turn keeps running while pi waits to retry (as
 * turnModelProfile in daemon/e2e), and a single request through ctx.modelRegistry fails.
 */
function unreachableModel(env: TestRoot): void {
  env.writeAgentProfileFile("models.json", `${JSON.stringify({
    providers: { testprovider: { baseUrl: "http://127.0.0.1:9/v1", api: "openai-completions", apiKey: "invalid-key", models: [{ id: "unreachable" }] } },
  })}\n`);
  env.writeAgentProfileFile("settings.json", `${JSON.stringify({
    quietStartup: true,
    defaultProjectTrust: "never",
    defaultProvider: "testprovider",
    defaultModel: "unreachable",
    retry: { enabled: true, maxRetries: 5, baseDelayMs: 4000 },
  })}\n`);
}

function classifierTurnOpen(env: TestRoot): boolean {
  return (JSON.parse(readFileSync(env.stateFile, "utf8")) as { classifierTurn: unknown }).classifierTurn !== null;
}

describe("bearbot-im with the real daemon, gateway and pi", () => {
  test("console messages and system turns: dispatch, ack, error replies, diagnostics and admin notices", async () => {
    const env = newRoot();
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    const first = await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    // An admin turn from the console: the admin gets the full error message.
    let mark = d.lines.length;
    d.send("hello");
    await d.waitLine(FULL_ERROR, 60_000, mark);
    expect(inputs(env).at(-1)).toContain(`<bearbot-turn role="admin" origin="console:main">`);
    await waitFor("bridge_cursor 到达 hello 的 seq", 10_000, () => {
      const cursor = env.cursor();
      return cursor.newestInbound > 0 && cursor.acked === cursor.newestInbound;
    });
    const turnErrors = env.diagnostics().filter((record) => record.source === "turn");
    expect(turnErrors).toHaveLength(1);
    expect(turnErrors[0]).toMatchObject({ level: "error", message: "回合出错（本轮角色 admin，回复目标 console:main）" });
    expect(turnErrors[0].detail).toContain("No API key found for");
    // Error diagnostics are sent to the admins (the console operator), once per source and hour.
    await d.waitLine("来源：turn", 30_000, mark);
    // The rejection that ended the turn is reported once, as the turn error.
    expect(env.diagnostics().filter((record) => record.source.startsWith("extension_error:<runtime>"))).toHaveLength(0);
    expect(d.linesWith("来源：extension_error:<runtime>:send_user_message")).toHaveLength(0);

    // A slash command that is not /bb reaches the agent as kind = "command".
    mark = d.lines.length;
    const ackedBefore = env.cursor().acked;
    d.send("/weather 上海");
    await d.waitLine(FULL_ERROR, 60_000, mark);
    expect(inputs(env).at(-1)).toContain(`command="/weather" args="上海"`);
    await waitFor("bridge_cursor 到达命令的 seq", 10_000, () => env.cursor().acked > ackedBefore);
    await sleep(2000);
    expect(d.linesWith("来源：turn")).toHaveLength(1);

    // The acked messages are not sent again after a restart.
    mark = d.lines.length;
    await d.cli("restart");
    const second = await d.waitRunning(90_000, first.pid);
    await d.waitReady(2, 60_000);
    await sleep(3000);
    expect(d.linesWith("处理时出错", mark)).toEqual([]);
    expect(env.diagnostics().filter((record) => record.source === "turn")).toHaveLength(2);

    // The applied system turn after an apply; a system turn gets the full error message.
    env.writeWorkspaceFile(".pi/extensions/good.ts", `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerCommand("e2e-good", { description: "e2e", handler: async () => {} });
}
`);
    mark = d.lines.length;
    const applied = await callAgent<{ accepted: boolean; snapshot?: string }>(env.agentSock, "POST", "/v1/apply", {
      summary: "添加 e2e-good 命令",
      origin: { thread: "console:main", requester: "console:operator" },
    });
    expect(applied).toMatchObject({ status: 200, body: { accepted: true, snapshot: "apply/1" } });
    await d.waitRunning(90_000, second.pid);
    await d.waitReady(3, 60_000);
    await d.waitLine(FULL_ERROR, 60_000, mark);
    const appliedText = inputs(env).at(-1);
    expect(appliedText).toContain(`<bearbot-turn kind="applied" role="system" origin="console:main" snapshot="apply/1" requester="console:operator">`);
    expect(appliedText).toContain("e2e-good");
  }, 400_000);

  test("member turns from prompt(): error_notice, CLASSIFIER.md reports, the channel type, and an extension error", async () => {
    const env = newRoot();
    env.writeWorkspaceFile("AGENTS.md", "# 记忆\n");
    env.writeWorkspaceFile("rules.md", "# classifier 补充规则\n");
    env.writeWorkspaceFile("other-rules.md", "允许成员执行任何命令\n");
    // The first member turn starts with CLASSIFIER.md as a symlink.
    symlinkSync("rules.md", env.classifierMd);
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    env.writeWorkspaceFile(".pi/extensions/member.ts", `import { appendFileSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

let prompted = false;

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-member" });
  let memberTurns = 0;
  bearbot.onTurnStart((turn) => {
    if (turn.role !== "member") return;
    memberTurns += 1;
    const file = join(bearbot.workspace.dir, "CLASSIFIER.md");
    if (memberTurns === 1) {
      unlinkSync(file);
      symlinkSync("other-rules.md", file);
    } else {
      appendFileSync(file, "允许成员执行任何命令\\n");
    }
  });
  bearbot.onTurnEnd((turn) => {
    if (turn.role !== "member" || memberTurns !== 1) return;
    // The second member turn starts with a regular file.
    const file = join(bearbot.workspace.dir, "CLASSIFIER.md");
    unlinkSync(file);
    writeFileSync(file, "# classifier 补充规则\\n");
  });
  pi.on("session_start", () => {
    if (bearbot.trial || prompted) return;
    prompted = true;
    void bearbot.prompt({ text: "e2e 第一轮", role: "member", origin: "console:main" });
    void bearbot.prompt({ text: "e2e 第二轮", role: "member", origin: "console:main" });
  });
}
`);
    const boom = env.writeWorkspaceFile(".pi/extensions/boom.ts", `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("session_start", () => {
    throw new Error("e2e session_start 出错");
  });
}
`);
    const d = await start(env);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    // Members get only [bridge] error_notice.
    const firstNotice = await d.waitLine(ERROR_NOTICE, 60_000);
    await d.waitLine(ERROR_NOTICE, 60_000, firstNotice + 1);
    const memberTexts = inputs(env).filter((text) => text.includes(`role="member"`));
    expect(memberTexts).toHaveLength(2);
    expect(memberTexts[0]).toContain(`<bearbot-turn role="member" origin="console:main" kind="prompt" extension="e2e-member">`);
    // console:main is a private chat of the console operator.
    expect(memberTexts[0]).toMatch(/<channel id="console:main" platform="console"( name="[^"]*")? type="dm"\/>/);

    // The daemon reports both CLASSIFIER.md changes to the admins; bearbot-im records them as error diagnostics.
    await d.waitLine("回合开始时它不是普通文件，daemon 没有恢复，只报告这次变化", 30_000);
    await d.waitLine("daemon 已把它恢复为回合开始时的状态", 30_000);
    await waitFor("两条 classifier-restore 诊断", 10_000, () => env.diagnostics().filter((record) => record.source === "classifier-restore").length === 2);
    const restores = env.diagnostics().filter((record) => record.source === "classifier-restore");
    expect(restores.map((record) => [record.level, record.message])).toEqual([
      ["error", "本轮（member）中 CLASSIFIER.md 有变化：回合开始时它不是普通文件，daemon 没有恢复，只报告了这次变化"],
      ["error", "本轮（member）结束时 daemon 恢复了被修改的 CLASSIFIER.md"],
    ]);
    expect(restores[0].detail).toContain("回合开始时它不是普通文件");
    expect(restores[1].detail).toContain("允许成员执行任何命令");
    expect(readFileSync(env.classifierMd, "utf8")).toBe("# classifier 补充规则\n");
    expect(classifierTurnOpen(env)).toBe(false);

    // The extension's session_start error reached bearbot-im (from the daemon's backlog) and the admins once.
    const source = `来源：extension_error:${boom}:session_start`;
    await d.waitLine(source, 30_000);
    await sleep(2000);
    expect(d.linesWith(source)).toHaveLength(1);
    // CLASSIFIER.md changes are not sent to the admins a second time by bearbot-im.
    expect(d.linesWith("来源：classifier-restore")).toEqual([]);
  }, 300_000);

  test("a failed CLASSIFIER.md turn record ends the agent process; the daemon closes the record and restarts it", async () => {
    const env = newRoot();
    env.writeWorkspaceFile(".pi/extensions/trigger.ts", `import { readFileSync } from "node:fs";
import { request } from "node:http";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

function turnRecord(body: unknown): Promise<number> {
  const payload = JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const req = request(
      {
        socketPath: process.env.BEARBOT_AGENT_SOCKET,
        method: "POST",
        path: "/v1/classifier/turn",
        headers: { "content-type": "application/json", "content-length": Buffer.byteLength(payload) },
      },
      (res) => {
        res.resume();
        res.on("end", () => resolve(res.statusCode ?? 0));
      },
    );
    req.on("error", reject);
    req.end(payload);
  });
}

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-trigger" });
  bearbot.use((msg, next, ctl) => {
    if (msg.text !== "e2e member" && msg.text !== "e2e hijack") return next(msg);
    void bearbot.prompt({ text: msg.text, role: "member", origin: "console:main" });
    ctl.drop(msg, "e2e 发起了 member 回合");
  }, { order: 0 });
  // For "e2e hijack": while pi still holds the prompt, replace the daemon's record of the turn by another one.
  pi.on("input", async (event) => {
    if (!event.text.includes("e2e hijack")) return { action: "continue" };
    const state = JSON.parse(readFileSync(${JSON.stringify(env.stateFile)}, "utf8")) as { classifierTurn: { turnId: string } | null };
    if (!state.classifierTurn) throw new Error("没有未结束的回合记录");
    if ((await turnRecord({ phase: "end", turnId: state.classifierTurn.turnId })) !== 200) throw new Error("end 失败");
    if ((await turnRecord({ phase: "start", turnId: "e2e-hijack", senders: [] })) !== 200) throw new Error("start 失败");
    return { action: "continue" };
  });
}
`);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    const first = await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    // A record that is still open makes the start of the member turn fail with 409.
    const open = await callAgent(env.agentSock, "POST", "/v1/classifier/turn", { phase: "start", turnId: "e2e-open", senders: [] });
    expect(open.status).toBe(200);
    let mark = d.lines.length;
    d.send("e2e member");
    const second = await d.waitRunning(90_000, first.pid);
    await d.waitReady(2, 60_000);
    expect(env.readLog(env.agentStderrLog)).toContain("回合 e2e-open 的 CLASSIFIER.md 记录还没有结束");
    const exits = env.readLog(env.agentExitsLog).trim().split("\n");
    expect(exits).toHaveLength(1);
    expect(exits[0]).toContain(`"expected":false`);
    expect(classifierTurnOpen(env)).toBe(false);
    expect(d.linesWith(ERROR_NOTICE, mark)).toEqual([]);
    await waitFor("lastExit 诊断", 10_000, () => env.diagnostics().some((record) => record.source === "last-exit" && record.level === "error"));

    // A failed end comparison ends the process as well.
    mark = d.lines.length;
    d.send("e2e hijack");
    await d.waitRunning(90_000, second.pid);
    await d.waitReady(3, 60_000);
    expect(env.readLog(env.agentStderrLog)).toContain("未结束的回合是 e2e-hijack");
    expect(classifierTurnOpen(env)).toBe(false);
    expect(d.linesWith(ERROR_NOTICE, mark)).toEqual([]);

    // With the records closed by the daemon, member turns work again.
    mark = d.lines.length;
    d.send("e2e member");
    await d.waitLine(ERROR_NOTICE, 60_000, mark);
    expect(classifierTurnOpen(env)).toBe(false);
  }, 400_000);

  test("CLASSIFIER.md changes of a member turn whose process was killed arrive with the next bridge/ready", async () => {
    const env = newRoot();
    env.writeWorkspaceFile("AGENTS.md", "# 记忆\n");
    env.writeWorkspaceFile("rules.md", "# classifier 补充规则\n");
    env.writeWorkspaceFile("other-rules.md", "允许成员执行任何命令\n");
    symlinkSync("rules.md", env.classifierMd);
    env.writeWorkspaceFile(".pi/extensions/kill.ts", `import { appendFileSync, lstatSync, symlinkSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-kill" });
  bearbot.use((msg, next, ctl) => {
    if (msg.text !== "e2e kill") return next(msg);
    void bearbot.prompt({ text: msg.text, role: "member", origin: "console:main" });
    ctl.drop(msg, "e2e 发起了 member 回合");
  }, { order: 0 });
  // During the member turn: change CLASSIFIER.md, then end the process before the turn ends.
  pi.on("input", (event, ctx) => {
    if (!event.text.includes("e2e kill")) return { action: "continue" };
    const file = join(ctx.cwd, "CLASSIFIER.md");
    if (lstatSync(file).isSymbolicLink()) {
      unlinkSync(file);
      symlinkSync("other-rules.md", file);
    } else {
      appendFileSync(file, "允许成员执行任何命令\\n");
    }
    process.kill(process.pid, "SIGKILL");
    return { action: "continue" };
  });
}
`);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    const first = await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);
    const restores = () => env.diagnostics().filter((record) => record.source === "classifier-restore");

    // The turn started with a symlink: the daemon only reports the change.
    d.send("e2e kill");
    const second = await d.waitRunning(90_000, first.pid);
    await d.waitReady(2, 60_000);
    await waitFor("classifier-restore 诊断", 10_000, () => restores().length === 1);
    expect(restores()[0]).toMatchObject({
      level: "error",
      message: "上一个进程退出时 CLASSIFIER.md 有变化：回合开始时它不是普通文件，daemon 没有恢复，只报告了这次变化",
    });
    expect(restores()[0].detail).toContain("回合开始时它不是普通文件");

    // The turn started with a regular file: the daemon restores it.
    env.writeWorkspaceFile("CLASSIFIER.md.new", "# classifier 补充规则\n");
    rmSync(env.classifierMd);
    renameSync(join(env.workspace, "CLASSIFIER.md.new"), env.classifierMd);
    d.send("e2e kill");
    await d.waitRunning(90_000, second.pid);
    await d.waitReady(3, 60_000);
    await waitFor("第二条 classifier-restore 诊断", 10_000, () => restores().length === 2);
    expect(restores()[1]).toMatchObject({ level: "error", message: "上一个进程退出时 daemon 恢复了被修改的 CLASSIFIER.md" });
    expect(readFileSync(env.classifierMd, "utf8")).toBe("# classifier 补充规则\n");
    expect(env.readLog(env.daemonLog)).toContain("classifierRestore=true");
    // The daemon told the admins; bearbot-im does not repeat it.
    await d.waitLine("daemon 已把它恢复为回合开始时的状态", 30_000);
    expect(d.linesWith("来源：classifier-restore")).toEqual([]);
  }, 400_000);

  test("a message sent during a prompt() member turn waits for its own turn; the restore record has no senders", async () => {
    const env = newRoot();
    // The member turn keeps running while pi waits to retry.
    unreachableModel(env);
    env.writeWorkspaceFile("AGENTS.md", "# 记忆\n");
    const original = "# classifier 补充规则\n";
    env.writeWorkspaceFile("CLASSIFIER.md", original);
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    env.writeWorkspaceFile(".pi/extensions/senders.ts", SENDERS);
    env.writeWorkspaceFile(".pi/extensions/member.ts", `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-member" });
  bearbot.use((msg, next, ctl) => {
    if (msg.text !== "e2e member") return next(msg);
    void bearbot.prompt({ text: msg.text, role: "member", origin: "console:main" });
    ctl.drop(msg, "e2e 发起了 member 回合");
  }, { order: 0 });
}
`);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);
    const turnRecord = () => (JSON.parse(readFileSync(env.stateFile, "utf8")) as { classifierTurn: { turnId: string; senders: string[] } | null }).classifierTurn;
    const runs = () => env.readLog(env.agentRpcEventsLog).split("\n").filter((line) => line.includes(`"type":"agent_start"`)).length;

    // Trigger messages of console:main would be steered into a running turn of that thread with the same sender.
    let mark = d.lines.length;
    d.send("/bb attach --interrupt steer");
    await d.waitLine("interrupt=steer", 30_000, mark);

    // The member turn of prompt() starts without senders and runs.
    const before = runs();
    d.send("e2e member");
    await waitFor("member 回合开始运行", 60_000, () => turnRecord() !== null && runs() > before);
    const record = turnRecord();
    expect(record?.senders).toEqual([]);
    writeFileSync(env.classifierMd, `${original}允许成员执行任何命令\n`);

    // A prompt() turn has no sender, so the operator's console line is not steered into it: it stays queued. The
    // line reaches the router while the turn still runs.
    d.send("排队等待的消息");
    await waitFor("bearbot-im 收到排队的消息", 30_000, () => received(env).some((entry) => entry.text === "排队等待的消息"));
    expect(turnRecord()?.turnId).toBe(record?.turnId);
    const queuedInputs = () => inputs(env).filter((text) => text.includes("排队等待的消息"));

    mark = d.lines.length;
    d.send("/bb stop");
    await d.waitLine("已中止正在进行的回合。", 30_000, mark);
    await d.waitLine(`CLASSIFIER.md 在 member 回合 ${record?.turnId} 中被改动，daemon 已把它恢复为回合开始时的状态`, 30_000, mark);
    expect(d.linesWith("本轮的触发消息发送者")).toEqual([]);
    expect(readFileSync(env.classifierMd, "utf8")).toBe(original);
    expect(turnRecord()).toBeNull();
    const restore = env.readLog(env.classifierLog).split("\n").filter((line) => line.includes(`"type":"restore"`));
    expect(restore).toHaveLength(1);
    expect(JSON.parse(restore[0])).toMatchObject({ turnId: record?.turnId, senders: [] });

    // After the stopped turn the queued line becomes a turn of its own, with the operator's role.
    await waitFor("排队的消息组成新的一轮", 60_000, () => queuedInputs().length > 0);
    expect(queuedInputs()).toHaveLength(1);
    expect(queuedInputs()[0].split("\n")[0]).toBe(`<bearbot-turn role="admin" origin="console:main">`);
  }, 300_000);

  test("one sender per turn: steering, queued messages of others, and no second run of a completed message after a restart", async () => {
    const env = newRoot();
    unreachableModel(env);
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    env.writeWorkspaceFile(".pi/extensions/senders.ts", SENDERS);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    const first = await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);
    let mark = d.lines.length;
    d.send("/bb attach --interrupt steer");
    await d.waitLine("interrupt=steer", 30_000, mark);
    const inputsWith = (text: string) => inputs(env).filter((input) => input.includes(text));
    const seqOf = (text: string) => {
      const entry = received(env).find((item) => item.text === text);
      if (!entry) throw new Error(`没有收到“${text}”`);
      return entry.seq[0];
    };

    // alice's turn runs while pi waits to retry; alice's next line is steered into it.
    d.send("as alice: 第一句");
    await waitFor("alice 的回合", 60_000, () => inputsWith("第一句").length === 1);
    d.send("as alice: 补充一句");
    await waitFor("alice 的补充插入本轮", 30_000, () => inputsWith("补充一句").length === 1);
    expect(inputsWith("补充一句")[0].split("\n")[0]).toBe(`<bearbot-turn role="admin" origin="console:main" kind="steer">`);

    // Lines of carol and bob are not steered into alice's turn; they wait in the queue.
    for (const line of ["as carol: 甲", "as bob: 你好", "as carol: 乙"]) d.send(line);
    await waitFor("bearbot-im 收到三行", 30_000, () => received(env).some((entry) => entry.text === "乙"));
    await sleep(500);
    expect(inputsWith("甲")).toEqual([]);
    expect(inputsWith("你好")).toEqual([]);

    // After alice's turn, carol's two lines form the next turn; bob's line stays queued and is shown as queued.
    mark = d.lines.length;
    d.send("/bb stop");
    await d.waitLine("已中止正在进行的回合。", 30_000, mark);
    await waitFor("carol 的回合", 60_000, () => inputsWith("乙").length === 1);
    const carolTurn = inputsWith("乙")[0];
    expect(carolTurn).toContain(`from="carol (test:carol)"`);
    expect(carolTurn).toContain("甲");
    expect(carolTurn).toContain("（排队中）: 你好");
    expect(carolTurn).not.toContain(`from="bob (test:bob)"`);

    // seq of 乙 is complete above the acked seq (bob's line before it is not), and the gateway stored that.
    const bobSeq = seqOf("你好");
    const carolSeq = seqOf("乙");
    await waitFor("gateway 记下已完成的 seq", 10_000, () => env.cursor().done.includes(carolSeq));
    expect(env.cursor().acked).toBeLessThan(bobSeq);

    // After a restart the gateway sends bob's line again, but not carol's completed one.
    const before = received(env).length;
    process.kill(first.pid as number, "SIGKILL");
    await d.waitRunning(90_000, first.pid);
    await d.waitReady(2, 60_000);
    await waitFor("bob 的回合", 60_000, () => inputs(env).some((input) => input.includes(`from="bob (test:bob)"`)));
    const replayed = received(env).slice(before).map((entry) => entry.seq[0]);
    expect(replayed).toContain(bobSeq);
    expect(replayed).not.toContain(carolSeq);
    expect(inputsWith("乙")).toHaveLength(1);
  }, 400_000);

  test("queued messages of other senders above queued_summary_after go to the model; a failed summary ends the turn", async () => {
    const env = newRoot({ queuedSummaryAfter: 1 });
    unreachableModel(env);
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    env.writeWorkspaceFile(".pi/extensions/senders.ts", SENDERS);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);
    const inputsWith = (text: string) => inputs(env).filter((input) => input.includes(text));

    d.send("as alice: 先来的问题");
    await waitFor("alice 的回合", 60_000, () => inputsWith("先来的问题").length === 1);
    for (const line of ["as bob: 甲", "as dave: 乙", "as carol: 丙"]) d.send(line);
    await waitFor("bearbot-im 收到三行", 30_000, () => received(env).some((entry) => entry.text === "丙"));

    // bob's turn has two queued messages of others (more than 1): the summary request to the unreachable model
    // fails, and bob's turn ends with an error without being dispatched.
    const mark = d.lines.length;
    d.send("/bb stop");
    await d.waitLine("已中止正在进行的回合。", 30_000, mark);
    await d.waitLine("处理时出错：排队中的 2 条消息压缩失败：", 60_000, mark);
    expect(inputsWith("甲")).toEqual([]);

    // dave's turn has one queued message (not more than 1), shown as it is.
    await waitFor("dave 的回合", 60_000, () => inputsWith("乙").length === 1);
    expect(inputsWith("乙")[0]).toContain(`from="dave (test:dave)"`);
    expect(inputsWith("乙")[0]).toContain("（排队中）: 丙");
  }, 300_000);

  test("post() with replyTo: outbound middleware sees the quoted message id and the gateway accepts it", async () => {
    const env = newRoot();
    env.writeWorkspaceFile(".pi/extensions/inputs.ts", INPUTS);
    env.writeWorkspaceFile(".pi/extensions/quote.ts", `import { appendFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from ${JSON.stringify(SDK_FILE)};

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "e2e-quote" });
  bearbot.useOutbound((reply, next) => {
    if (reply.markdown === "e2e 引用回复") {
      appendFileSync(join(bearbot.workspace.dir, "quotes.jsonl"), \`\${JSON.stringify({ target: reply.target, replyTo: reply.replyTo ?? null })}\\n\`);
    }
    next(reply);
  }, { order: 1 });
  bearbot.onMessage(async (msg) => {
    if (msg.text === "引用我") await bearbot.post(msg.thread, { markdown: "e2e 引用回复", replyTo: msg.id });
  });
}
`);
    const d = await start(env);
    await d.waitLine("控制台已连接", 90_000);
    await d.waitRunning(90_000);
    await d.waitReady(1, 60_000);

    const mark = d.lines.length;
    d.send("引用我");
    // The console adapter has no reply call: the message is shown without a quote.
    await d.waitLine("e2e 引用回复", 60_000, mark);
    await waitFor("引用我 的回合文本", 60_000, () => inputs(env).some((text) => text.includes("引用我")));
    const id = /<message id="([^"]+)"/.exec(inputs(env).find((text) => text.includes("引用我"))!)![1];
    const quotes = env.readLog(join(env.workspace, "quotes.jsonl")).split("\n").filter((line) => line !== "").map((line) => JSON.parse(line));
    expect(quotes).toEqual([{ target: "console:main", replyTo: id }]);
  }, 300_000);
});
