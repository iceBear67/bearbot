// Console interactive mode (DESIGN 6.2.4) on a real pseudo terminal: util-linux `script` runs the gateway
// with the pty as fds 3 and 4, the test types into the pty master. Screen contents are checked in tmux, a real
// terminal emulator.
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeAll, describe, expect, test } from "vitest";
import {
  GATEWAY_TZ,
  MAIN_JS,
  connectBridge,
  connectDaemon,
  makeEnv,
  removeEnv,
  requireBuild,
  sleep,
  stripAnsi,
  waitFor,
  type Env,
  type RpcPeer,
} from "./helpers.ts";

const PTY_ENV = { PATH: process.env.PATH ?? "/usr/bin:/bin", TZ: GATEWAY_TZ, TERM: "xterm" };
const HINT = "docker attach 中按 Ctrl-P Ctrl-Q 断开；停止 bearbot 请执行 docker stop";

function gatewayLog(env: Env): string {
  const file = `${env.root}/gateway.log`;
  return existsSync(file) ? readFileSync(file, "utf8") : "";
}

/** pid of the gateway from its "gateway starting" log line. */
function gatewayPid(env: Env): number {
  const m = /"pid":(\d+)[^\n]*"msg":"gateway starting"/.exec(gatewayLog(env));
  if (!m) {
    throw new Error(`no "gateway starting" line in ${env.root}/gateway.log`);
  }
  return Number(m[1]);
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw e;
  }
}

class PtyGateway {
  readonly child: ChildProcess;
  raw = "";
  readonly exited: Promise<void>;

  /** `ownSession`: the gateway runs in its own session without a controlling terminal, as bearbotd starts it. */
  constructor(
    readonly env: Env,
    readonly ownSession = false,
  ) {
    const log = `${env.root}/gateway.log`;
    const gateway = `${process.execPath} ${MAIN_JS} --config ${env.configPath} 3<&0 4>&1 >${log} 2>&1`;
    const cmd = ownSession ? `exec setsid -w ${gateway}` : `exec ${gateway}`;
    this.child = spawn("script", ["-qfec", cmd, "/dev/null"], {
      stdio: ["pipe", "pipe", "pipe"],
      env: { ...PTY_ENV, HOME: env.dataDir },
    });
    this.child.stdout!.on("data", (d: Buffer) => (this.raw += d.toString("utf8")));
    this.exited = new Promise((r) => this.child.on("exit", () => r()));
  }

  get screen(): string {
    return stripAnsi(this.raw);
  }

  keys(s: string): void {
    this.child.stdin!.write(s);
  }

  async waitScreen(pattern: RegExp, from = 0): Promise<RegExpMatchArray> {
    return waitFor(`screen ${pattern} in ${JSON.stringify(this.raw.slice(from))}`, () => stripAnsi(this.raw.slice(from)).match(pattern) ?? undefined);
  }

  async stop(): Promise<void> {
    if (this.ownSession && existsSync(`${this.env.root}/gateway.log`)) {
      const pid = gatewayPid(this.env);
      if (alive(pid)) {
        process.kill(pid, "SIGKILL");
        await waitFor(`gateway ${pid} gone`, () => (alive(pid) ? undefined : true));
      }
    }
    this.child.kill("SIGTERM");
    await this.exited;
  }
}

/** The gateway in a tmux pane of a fixed size; `screen` is what the terminal shows. */
class TmuxGateway {
  private readonly socket: string;

  constructor(env: Env, cols: number, rows: number) {
    this.socket = join(env.root, "tmux.sock");
    const cmd = `exec ${process.execPath} ${MAIN_JS} --config ${env.configPath} 3<&0 4>&1 >${env.root}/gateway.log 2>&1`;
    this.tmux(["-f", "/dev/null", "new-session", "-d", "-s", "gw", "-x", String(cols), "-y", String(rows), cmd], { ...PTY_ENV, HOME: env.dataDir });
  }

  private tmux(args: string[], env?: NodeJS.ProcessEnv): string {
    return execFileSync("tmux", ["-S", this.socket, ...args], { encoding: "utf8", env: env ?? PTY_ENV });
  }

  /**
   * Rows of the screen without trailing spaces and trailing empty rows (capture-pane drops them too); clock times
   * and message numbers replaced by T and #N.
   */
  rows(): string[] {
    const rows = this.tmux(["capture-pane", "-p", "-t", "gw"])
      .split("\n")
      .map((r) => r.trimEnd().replace(/\d{2}:\d{2}:\d{2}/, "T").replace(/#\d+/, "#N"));
    while (rows.length > 0 && rows.at(-1) === "") rows.pop();
    return rows;
  }

  async waitRows(expected: string[]): Promise<void> {
    const deadline = Date.now() + 10_000;
    for (;;) {
      const rows = this.rows();
      if (JSON.stringify(rows) === JSON.stringify(expected)) return;
      if (Date.now() > deadline) {
        expect(rows).toEqual(expected);
      }
      await sleep(50);
    }
  }

  /** Literal text, written to the terminal in one piece (like a paste without bracketed paste). */
  text(s: string): void {
    this.tmux(["send-keys", "-t", "gw", "-l", s]);
  }

  /** One key press per character. */
  typeKeys(s: string): void {
    for (const c of s) this.tmux(["send-keys", "-t", "gw", "-l", c]);
  }

  key(name: string): void {
    this.tmux(["send-keys", "-t", "gw", name]);
  }

  /** Ends the pane and its gateway; nothing to do when the server already exited with its only pane. */
  kill(): void {
    if (spawnSync("tmux", ["-S", this.socket, "has-session"], { env: PTY_ENV }).status === 0) {
      this.tmux(["kill-server"]);
    }
  }
}

beforeAll(() => requireBuild());

const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  while (cleanups.length > 0) await cleanups.pop()!();
});

describe("interactive console (util-linux script provides the pty)", () => {
  beforeAll(() => {
    if (spawnSync("script", ["--version"]).status !== 0) {
      throw new Error('util-linux "script" is required for the interactive console tests (it provides the pty); install util-linux');
    }
  });

  async function start(ownSession = false): Promise<{ env: Env; pty: PtyGateway; peer: RpcPeer; ctl: RpcPeer }> {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const pty = new PtyGateway(env, ownSession);
    cleanups.push(() => pty.stop());
    await pty.waitScreen(/\[\d{2}:\d{2}:\d{2} 状态\] 控制台已连接\n> /);
    expect(pty.raw).toContain("\x1b[?2004h");
    await waitFor("sockets", () => (existsSync(env.gatewaySocket) ? true : undefined));
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const { peer } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    return { env, pty, peer, ctl };
  }

  test("lines, bracketed paste, backslash continuation, Ctrl-C, Ctrl-D, empty lines", async () => {
    const { pty, peer } = await start();
    pty.keys("hello\r");
    await peer.waitMessage((m) => m.text === "hello");

    pty.keys("\x1b[200~第一行\r第二行\r第三行\x1b[201~\r");
    await peer.waitMessage((m) => m.text === "第一行\n第二行\n第三行");

    let from = pty.raw.length;
    pty.keys("abc\\\r");
    await pty.waitScreen(/\.\.\. $/, from);
    pty.keys("def\r");
    await peer.waitMessage((m) => m.text === "abc\ndef");

    from = pty.raw.length;
    pty.keys("被丢弃的内容");
    pty.keys("\x03");
    await pty.waitScreen(/docker attach 中按 Ctrl-P Ctrl-Q 断开；停止 bearbot 请执行 docker stop\n> $/, from);
    pty.keys("x\r");
    await peer.waitMessage((m) => m.text === "x");

    from = pty.raw.length;
    pty.keys("\x04");
    await pty.waitScreen(/docker attach 中按 Ctrl-P Ctrl-Q 断开/, from);
    pty.keys("after ctrl-d\r");
    await peer.waitMessage((m) => m.text === "after ctrl-d");

    pty.keys("\r\r   \r");
    pty.keys("last\r");
    await peer.waitMessage((m) => m.text === "last");
    expect(peer.messages().map((m) => m.text)).toEqual(["hello", "第一行\n第二行\n第三行", "abc\ndef", "x", "after ctrl-d", "last"]);
  });

  test("output arriving while typing keeps the line being edited; typing prompt", async () => {
    const { pty, peer } = await start();
    pty.keys("正在输入");
    await pty.waitScreen(/> 正在输入$/);
    let from = pty.raw.length;
    const { ref } = await peer.request("post", { target: "console:main", markdown: "agent 的回复" });
    const n = ref.split("-").at(-1);
    await pty.waitScreen(new RegExp(`\\[\\d{2}:\\d{2}:\\d{2} bearbot #${n}\\]\\nagent 的回复\\n> 正在输入$`), from);
    pty.keys("\r");
    await peer.waitMessage((m) => m.text === "正在输入");

    from = pty.raw.length;
    await peer.request("typing", { target: "console:main" });
    await pty.waitScreen(/\(处理中\) > $/, from);
    from = pty.raw.length;
    await peer.request("post", { target: "console:main", markdown: "完成" });
    await pty.waitScreen(/完成\n> $/, from);
    await sleep(100);
    expect(pty.screen.slice(-12)).not.toContain("处理中");
  });

  test("bracketed paste is turned on again before each fresh prompt (a terminal attached later)", async () => {
    const { pty } = await start();
    let from = pty.raw.length;
    // DESIGN 5.13: after docker attach the operator presses Enter to get a prompt.
    pty.keys("\r");
    await waitFor("\\x1b[?2004h after Enter", () => (pty.raw.slice(from).includes("\x1b[?2004h") ? true : undefined));
    from = pty.raw.length;
    pty.keys("\x03");
    await pty.waitScreen(new RegExp(`${HINT}\\n> $`), from);
    expect(pty.raw.slice(from)).toContain("\x1b[?2004h");
    from = pty.raw.length;
    pty.keys("\x04");
    await pty.waitScreen(new RegExp(`${HINT}\\n> $`), from);
    expect(pty.raw.slice(from)).toContain("\x1b[?2004h");
  });

  test("Ctrl-Z keeps the terminal in raw mode: a Ctrl-C after it only shows the hint", async () => {
    const { env, pty, peer } = await start();
    let from = pty.raw.length;
    pty.keys("\x1a");
    await pty.waitScreen(new RegExp(`${HINT}\\n> $`), from);
    from = pty.raw.length;
    // In cooked mode this Ctrl-C would be SIGINT for the terminal's foreground process group (here the gateway,
    // in the container bearbotd, which shuts down on SIGINT).
    pty.keys("\x03");
    await pty.waitScreen(new RegExp(`${HINT}\\n> $`), from);
    pty.keys("after ctrl-z\r");
    await peer.waitMessage((m) => m.text === "after ctrl-z");
    expect(gatewayLog(env)).not.toMatch(/"msg":"shutting down"/);
    // Raw mode: the typed text was echoed once, by readline.
    expect(pty.screen.split("after ctrl-z").length - 1).toBe(1);
  });

  test("the input history (up arrow) survives Ctrl-D on an empty line", async () => {
    const { pty, peer } = await start();
    pty.keys("first\r");
    await peer.waitMessage((m) => m.text === "first");
    const from = pty.raw.length;
    pty.keys("\x04");
    await pty.waitScreen(new RegExp(`${HINT}\\n> $`), from);
    pty.keys("\x1b[A");
    await pty.waitScreen(/> first$/, from);
    pty.keys("\r");
    await waitFor("first sent again", () => (peer.messages().filter((m) => m.text === "first").length === 2 ? true : undefined));
  });

  test("a terminal hangup ends the console input; the gateway keeps running", async () => {
    const { env, pty, ctl } = await start(true);
    const pid = gatewayPid(env);
    // The terminal goes away: script dies and the pty master is closed.
    pty.child.kill("SIGKILL");
    await pty.exited;
    let status: any;
    const deadline = Date.now() + 10_000;
    for (;;) {
      const { platforms } = await ctl.request("platforms.status", {});
      status = platforms[0];
      if (status.state === "input-ended" || Date.now() > deadline) break;
      await sleep(50);
    }
    expect(status).toMatchObject({ name: "console", state: "input-ended", lastError: expect.stringMatching(/EIO/) });
    await sleep(500);
    expect(alive(pid)).toBe(true);
    expect(await ctl.request("admin.list", {})).toMatchObject({ admins: [expect.objectContaining({ identity: "console:operator" })] });
    expect(gatewayLog(env)).not.toMatch(/Unhandled 'error' event/);
  });
});

describe("interactive console screen (tmux as the terminal)", () => {
  beforeAll(() => {
    if (spawnSync("tmux", ["-V"]).status !== 0) {
      throw new Error("tmux is required for the console screen tests (it is the terminal emulator); install tmux");
    }
  });

  test("output while a line wider than the terminal is edited: every block stays complete, the line is drawn once below", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const tm = new TmuxGateway(env, 40, 30);
    cleanups.push(() => tm.kill());
    await waitFor("prompt", () => (tm.rows().at(-1) === ">" ? true : undefined));
    await waitFor("sockets", () => (existsSync(env.gatewaySocket) ? true : undefined));
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const { peer } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    // "> " and 62 characters: two rows in 40 columns.
    const line = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ";
    const edited = ["> abcdefghijklmnopqrstuvwxyz0123456789AB", "CDEFGHIJKLMNOPQRSTUVWXYZ"];
    const start = ["[T 状态] 控制台已连接", "[T 状态] agent：running"];
    await tm.waitRows([...start, ">"]);

    // The line arrives in one piece, so readline redraws nothing while it is entered.
    tm.text(line);
    await tm.waitRows([...start, ...edited]);
    await peer.request("post", { target: "console:main", markdown: "reply-one\nreply-two" });
    await peer.request("post", { target: "console:main", markdown: "reply-three" });
    const replies = ["[T bearbot #N]", "reply-one", "reply-two", "[T bearbot #N]", "reply-three"];
    await tm.waitRows([...start, ...replies, ...edited]);
    // The longer typing prompt (11 columns), then a reply that turns it off.
    await peer.request("typing", { target: "console:main" });
    await tm.waitRows([...start, ...replies, "(处理中) > abcdefghijklmnopqrstuvwxyz012", "3456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"]);
    await peer.request("post", { target: "console:main", markdown: "reply-four" });
    const four = ["[T bearbot #N]", "reply-four"];
    await tm.waitRows([...start, ...replies, ...four, ...edited]);
    tm.key("Enter");
    await peer.waitMessage((m) => m.text === line);
    await tm.waitRows([...start, ...replies, ...four, ...edited, ">"]);

    // The same line key by key: readline redraws after every key.
    tm.typeKeys(line);
    await tm.waitRows([...start, ...replies, ...four, ...edited, ...edited]);
    await peer.request("post", { target: "console:main", markdown: "reply-five" });
    await peer.request("post", { target: "console:main", markdown: "reply-six" });
    await tm.waitRows([...start, ...replies, ...four, ...edited, "[T bearbot #N]", "reply-five", "[T bearbot #N]", "reply-six", ...edited]);
    // Editing in the middle of the line still lands at the cursor.
    for (let i = 0; i < 26; i++) tm.key("Left");
    tm.text("XX");
    tm.key("Enter");
    await peer.waitMessage((m) => m.text === "abcdefghijklmnopqrstuvwxyz0123456789XXABCDEFGHIJKLMNOPQRSTUVWXYZ");
  });
});
