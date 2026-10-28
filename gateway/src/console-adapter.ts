import { createReadStream, createWriteStream, fstatSync, statSync } from "node:fs";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { Socket } from "node:net";
import { isAbsolute, join, resolve } from "node:path";
import * as readline from "node:readline";
import { StringDecoder } from "node:string_decoder";
import * as tty from "node:tty";
import type { Readable, Writable } from "node:stream";
import {
  Message,
  NotImplementedError,
  convertEmojiPlaceholders,
  defaultEmojiResolver,
  parseMarkdown,
  stringifyMarkdown,
  type Adapter,
  type AdapterPostableMessage,
  type Attachment,
  type Author,
  type CardElement,
  type ChatInstance,
  type EmojiValue,
  type FetchOptions,
  type FetchResult,
  type FileUpload,
  type FormattedContent,
  type RawMessage,
  type ThreadInfo,
  type WebhookOptions,
} from "chat";
import {
  extractCard,
  extractFiles,
  extractPostableAttachments,
  toBuffer,
  type PlatformName,
} from "@chat-adapter/shared";
import mime from "mime";
import { errorText, type PinoLogger } from "./log.ts";
import { StatusRecorder } from "./platform-status.ts";
import { CONSOLE_CHANNEL } from "./repo.ts";
import { clock, localDate } from "./time.ts";

export type ConsoleRaw = {
  id: string;
  text: string;
  time: number;
  file?: { path: string; name: string; size: number; mimeType?: string };
};

export type ConsoleAdapterOptions = {
  inputFd: number;
  outputFd: number;
  /** <GatewayDataDir>/console-files */
  filesDir: string;
  /** Base of relative `:file` paths. */
  workspaceDir: string;
  userName: string;
  logger: PinoLogger;
};

const OPERATOR: Author = { userId: "operator", userName: "operator", fullName: "控制台", isBot: false, isMe: false };

const PROMPT = "> ";
const CONTINUATION_PROMPT = "... ";
const TYPING_PREFIX = "(处理中) ";
const TYPING_RESET_MS = 10_000;
const DETACH_HINT = "docker attach 中按 Ctrl-P Ctrl-Q 断开；停止 bearbot 请执行 docker stop";

const HELP = [
  "本地命令：",
  "  :help                 显示本说明",
  "  :file <路径> [文字]   发送一个文件，文字作为这条消息的正文；路径为绝对路径或相对工作区目录的路径",
  "  ::<文字>              发送以 : 开头的普通消息",
  "其他输入：",
  "  以 / 开头的输入作为命令发送，/bb help 查看管理命令",
  "  行尾为 \\ 时继续输入下一行，合并为一条消息",
  `  Ctrl-C 清除正在输入的内容；${DETACH_HINT}`,
].join("\n");

const SHARED: { platform: PlatformName } = { platform: "console" as unknown as PlatformName };

function attachmentType(mimeType: string | undefined): Attachment["type"] {
  if (mimeType?.startsWith("image/")) return "image";
  if (mimeType?.startsWith("audio/")) return "audio";
  if (mimeType?.startsWith("video/")) return "video";
  return "file";
}

/** File names are written as given, with path separators replaced. */
export function safeFileName(name: string): string {
  const cleaned = name.replace(/[/\\\0]/g, "_");
  return cleaned === "" || cleaned === "." || cleaned === ".." ? "_" : cleaned;
}

/** "#12" in console output: the counter part of "<start ms>-<n>". */
function seqPart(messageId: string): string {
  const m = /^\d+-(\d+)$/.exec(messageId);
  return m ? m[1]! : messageId;
}

function emojiText(emoji: EmojiValue | string): string {
  return defaultEmojiResolver.toGChat(emoji);
}

/**
 * Line mode streams. Pipes and sockets use net.Socket like Node's own stdio: an fs stream would keep a
 * threadpool thread blocked in read(2) on an idle pipe, and process exit waits for that thread.
 * Regular files and other devices use fs streams.
 */
function isStreamFd(fd: number): boolean {
  const st = fstatSync(fd);
  return st.isFIFO() || st.isSocket();
}

function openInput(fd: number): Readable {
  return isStreamFd(fd) ? new Socket({ fd, readable: true, writable: false }) : createReadStream("", { fd });
}

function openOutput(fd: number): Writable {
  return isStreamFd(fd) ? new Socket({ fd, readable: false, writable: true }) : createWriteStream("", { fd });
}

function indent(text: string): string {
  return text
    .split("\n")
    .map((l) => `  ${l}`)
    .join("\n");
}

/**
 * Chat adapter on the terminal bearbotd hands over as fds 3 and 4 (DESIGN 5.13, 6.2.4).
 * One thread `console:main`, always a DM, sender `console:operator`.
 */
export class ConsoleAdapter implements Adapter<Record<string, never>, ConsoleRaw> {
  readonly name = "console";
  readonly userName: string;
  readonly persistThreadHistory = true;
  readonly status: StatusRecorder;
  readonly interactive: boolean;

  private chat: ChatInstance | null = null;
  private readonly idBase = Date.now();
  private counter = 0;
  private input: Readable | null = null;
  private output: Writable | null = null;
  private rl: readline.Interface | null = null;
  /** Input history (up/down arrows), kept for the readline instance recreated after Ctrl-D. */
  private history: string[] = [];
  private pasting = false;
  private pasteLines: string[] = [];
  private continuation: string[] = [];
  private typing = false;
  private typingTimer: NodeJS.Timeout | null = null;
  private inputStopped = false;
  /** The first error of the console output; later output is not written. */
  private outputError: Error | null = null;
  private queue: Promise<void> = Promise.resolve();

  constructor(private readonly options: ConsoleAdapterOptions) {
    this.userName = options.userName;
    this.interactive = tty.isatty(options.inputFd);
    this.status = new StatusRecorder("console", this.interactive ? "interactive" : "line");
  }

  async initialize(chat: ChatInstance): Promise<void> {
    this.chat = chat;
    if (this.interactive) {
      const input = new tty.ReadStream(this.options.inputFd);
      const output = new tty.WriteStream(this.options.outputFd);
      this.input = input;
      this.output = output;
      this.watchOutput(output);
      // Registered before any readline instance, so an ended input is known before readline's own 'close'.
      input.on("end", () => this.inputEnded());
      // While a readline instance is attached, it passes input errors on as its own 'error' event (createInterface).
      input.on("error", (e) => {
        if (this.rl === null) {
          this.inputFailed(e);
        }
      });
      input.on("keypress", (_s: string | undefined, key: readline.Key | undefined) => {
        if (key?.name === "paste-start") {
          this.pasting = true;
        } else if (key?.name === "paste-end") {
          this.pasting = false;
        }
      });
      // createInterface turns bracketed paste on before the first prompt.
      this.rl = this.createInterface();
      this.printLine(`[${clock(new Date())} 状态] 控制台已连接`);
    } else {
      const input = openInput(this.options.inputFd);
      const output = openOutput(this.options.outputFd);
      this.input = input;
      this.output = output;
      this.watchOutput(output);
      this.printLine(`[${clock(new Date())} 状态] 控制台已连接`);
      this.readLines(input);
    }
  }

  async disconnect(): Promise<void> {
    this.stopInput();
  }

  /** Stops reading input (drain); output keeps working. */
  stopInput(): void {
    if (this.inputStopped) {
      return;
    }
    this.inputStopped = true;
    if (this.rl) {
      const rl = this.rl;
      this.rl = null;
      this.clearEditArea(rl, this.output!);
      rl.close();
      this.output!.write("\x1b[?2004l");
    }
    this.input?.destroy();
  }

  // ---- input ----

  private createInterface(): readline.Interface {
    const rl = readline.createInterface({
      input: this.input!,
      output: this.output!,
      terminal: true,
      prompt: PROMPT,
      history: this.history,
    });
    rl.on("history", (history: string[]) => {
      this.history = history;
    });
    rl.setPrompt(this.promptText());
    rl.on("line", (line) => this.onInteractiveLine(line));
    rl.on("SIGINT", () => {
      this.continuation = [];
      this.pasteLines = [];
      this.pasting = false;
      rl.write(null, { ctrl: true, name: "e" });
      rl.write(null, { ctrl: true, name: "u" });
      rl.setPrompt(this.promptText());
      this.enableBracketedPaste();
      this.printLine(DETACH_HINT);
    });
    // With a listener readline only reports Ctrl-Z: it keeps the terminal in raw mode and does not stop the
    // process, so a later Ctrl-C still cannot reach bearbotd as SIGINT (DESIGN 6.2.4).
    rl.on("SIGTSTP", () => this.printLine(DETACH_HINT));
    // readline passes errors of the input on (for example setRawMode EIO after the terminal hung up).
    rl.on("error", (e) => this.inputFailed(e));
    rl.on("close", () => {
      if (this.rl !== rl) {
        return;
      }
      this.rl = null;
      if (this.inputStopped || this.input!.readableEnded || this.input!.destroyed) {
        return;
      }
      // Ctrl-D on an empty line: keep the console usable.
      this.continuation = [];
      this.pasteLines = [];
      this.rl = this.createInterface();
      this.printLine(DETACH_HINT);
    });
    this.enableBracketedPaste();
    rl.prompt();
    return rl;
  }

  /**
   * Turns bracketed paste on before a fresh prompt. `docker attach` only passes on output written after the
   * terminal attached, so a terminal attached later never got the sequence written at startup (DESIGN 5.13).
   */
  private enableBracketedPaste(): void {
    if (this.outputError === null) {
      this.output!.write("\x1b[?2004h");
    }
  }

  private onInteractiveLine(line: string): void {
    if (this.inputStopped) {
      return;
    }
    if (this.pasting) {
      this.pasteLines.push(line);
      return;
    }
    if (this.pasteLines.length > 0) {
      const merged = line === "" ? this.pasteLines.join("\n") : [...this.pasteLines, line].join("\n");
      this.pasteLines = [];
      this.acceptLine(merged);
    } else {
      this.acceptLine(line);
    }
    if (this.rl) {
      this.rl.setPrompt(this.promptText());
      this.enableBracketedPaste();
      this.rl.prompt();
    }
  }

  private readLines(input: Readable): void {
    const decoder = new StringDecoder("utf8");
    let buffer = "";
    input.on("data", (chunk: Buffer) => {
      if (this.inputStopped) {
        return;
      }
      buffer += decoder.write(chunk);
      let nl: number;
      while ((nl = buffer.indexOf("\n")) >= 0) {
        let line = buffer.slice(0, nl);
        buffer = buffer.slice(nl + 1);
        if (line.endsWith("\r")) {
          line = line.slice(0, -1);
        }
        this.acceptLine(line);
      }
    });
    input.on("end", () => {
      buffer += decoder.end();
      if (buffer !== "" && !this.inputStopped) {
        this.acceptLine(buffer);
      }
      buffer = "";
      this.flushContinuation();
      this.inputEnded();
    });
    input.on("error", (e) => this.inputFailed(e));
  }

  private inputEnded(): void {
    this.status.set("input-ended");
    this.options.logger.info("console input ended");
  }

  private inputFailed(e: Error): void {
    this.options.logger.error({ err: e }, "console input failed");
    this.status.recordError(e);
    this.status.set("input-ended");
  }

  /** Backslash continuation (DESIGN 6.2.4), then submission. */
  private acceptLine(line: string): void {
    if (line.endsWith("\\")) {
      this.continuation.push(line.slice(0, -1));
      return;
    }
    const text = [...this.continuation, line].join("\n");
    this.continuation = [];
    this.submit(text);
  }

  private flushContinuation(): void {
    if (this.continuation.length > 0) {
      const text = this.continuation.join("\n");
      this.continuation = [];
      this.submit(text);
    }
  }

  private submit(text: string): void {
    if (text.trim() === "") {
      return;
    }
    // Inputs are processed one after another, in the order they were typed.
    this.queue = this.queue.then(() => this.process(text));
  }

  private async process(text: string): Promise<void> {
    if (text.startsWith("::")) {
      await this.sendMessage(text.slice(1));
      return;
    }
    if (text.startsWith(":")) {
      await this.localCommand(text);
      return;
    }
    const trimmed = text.trimStart();
    const slash = /^(\/\S*)(?:\s+([\s\S]*))?$/.exec(trimmed);
    if (slash && slash[1] !== "/") {
      await this.sendCommand(slash[1]!, slash[2] ?? "", trimmed);
      return;
    }
    await this.sendMessage(text);
  }

  private async localCommand(text: string): Promise<void> {
    const head = /^(:\S*)/.exec(text)![1]!;
    if (head === ":help" && text.trim() === ":help") {
      this.printLine(HELP);
      return;
    }
    if (head === ":file") {
      const m = /^:file\s+(?:"([^"]*)"|'([^']*)'|(\S+))(?:\s+([\s\S]*))?$/.exec(text);
      const given = m ? (m[1] ?? m[2] ?? m[3])! : "";
      if (!m || given === "") {
        this.printError("用法：:file <路径> [文字]");
        return;
      }
      const path = isAbsolute(given) ? given : resolve(this.options.workspaceDir, given);
      let size: number;
      try {
        const st = statSync(path);
        if (!st.isFile()) {
          this.printError(`不是普通文件：${path}`);
          return;
        }
        size = st.size;
      } catch (e) {
        this.printError(`无法读取文件 ${path}：${errorText(e)}`);
        return;
      }
      const name = path.slice(path.lastIndexOf("/") + 1);
      await this.sendMessage(m[4] ?? "", { path, name, size, mimeType: mime.getType(name) ?? undefined });
      return;
    }
    this.printError(`未知的本地命令：${head}，输入 :help 查看说明`);
  }

  private nextId(): string {
    this.counter += 1;
    return `${this.idBase}-${this.counter}`;
  }

  private async sendMessage(text: string, file?: ConsoleRaw["file"]): Promise<void> {
    const raw: ConsoleRaw = { id: this.nextId(), text, time: Date.now(), file };
    await this.dispatch(() => this.chat!.processMessage(this, CONSOLE_CHANNEL, this.parseMessage(raw)));
  }

  private async sendCommand(command: string, args: string, full: string): Promise<void> {
    const raw: ConsoleRaw = { id: this.nextId(), text: full, time: Date.now() };
    await this.dispatch(() =>
      this.chat!.processSlashCommand(
        { adapter: this, channelId: CONSOLE_CHANNEL, command, text: args, user: OPERATOR, raw },
        undefined,
      ),
    );
  }

  private async dispatch(run: () => Promise<void> | void): Promise<void> {
    try {
      await run();
    } catch (e) {
      // chat-sdk has logged the handler error already; the operator sees it here.
      this.printError(`消息处理失败：${errorText(e)}`);
    }
  }

  // ---- output ----

  private watchOutput(output: Writable): void {
    // The listener keeps a write error from becoming an uncaught exception that would stop the IM platforms too.
    output.on("error", (e) => {
      this.status.recordError(e);
      if (this.outputError === null) {
        this.outputError = e;
        this.options.logger.error({ err: e }, "console output failed, sending to the console fails from now on");
      }
    });
  }

  private promptText(): string {
    return (this.typing ? TYPING_PREFIX : "") + (this.continuation.length > 0 ? CONTINUATION_PROMPT : PROMPT);
  }

  /**
   * Clears every row of the prompt and the line being edited: a line wider than the terminal wraps over several
   * rows, and the cursor may be on any of them. Called while the prompt of the drawn line is still set.
   */
  private clearEditArea(rl: readline.Interface, out: Writable): void {
    readline.moveCursor(out, 0, -rl.getCursorPos().rows);
    readline.cursorTo(out, 0);
    readline.clearScreenDown(out);
  }

  /** Draws the prompt and the line being edited, starting on the row of the cursor. */
  private redrawPrompt(rl: readline.Interface): void {
    // readline's redraw first moves up by the rows its last redraw left the cursor on. The prompt now starts on
    // the current row, so that count is 0, the value readline sets itself after Enter.
    (rl as readline.Interface & { prevRows: number }).prevRows = 0;
    rl.setPrompt(this.promptText());
    rl.prompt(true);
  }

  /** Writes a block above the prompt and redraws the prompt with the line being edited. */
  private writeBlock(out: Writable, text: string, done?: (e: Error | null | undefined) => void): void {
    if (this.rl) {
      this.clearEditArea(this.rl, out);
    }
    out.write(text.endsWith("\n") ? text : `${text}\n`, done);
    if (this.rl) {
      this.redrawPrompt(this.rl);
    }
  }

  /** Output nobody waits for (states, local errors, hints); not written after the output failed. */
  printLine(text: string): void {
    if (this.outputError !== null || !this.output) {
      return;
    }
    this.writeBlock(this.output, text);
  }

  /**
   * Output of the send methods: resolves when the block is written, rejects when the console output failed, so
   * the sender gets the failure (DESIGN 6.2.2: bearbot-im reports it as a diagnostic).
   */
  private async sendBlock(text: string): Promise<void> {
    const out = this.output;
    if (!out) {
      throw new Error("the console adapter is not initialized");
    }
    if (this.outputError !== null) {
      throw new Error(`控制台输出已失败：${errorText(this.outputError)}`);
    }
    await new Promise<void>((resolve, reject) => {
      this.writeBlock(out, text, (e) => (e ? reject(e) : resolve()));
    });
  }

  printStatus(text: string): void {
    this.printLine(`[${clock(new Date())} 状态] ${text}`);
  }

  printError(text: string): void {
    this.printLine(`[${clock(new Date())} 错误] ${text}`);
  }

  private setTyping(on: boolean): void {
    if (this.typingTimer) {
      clearTimeout(this.typingTimer);
      this.typingTimer = null;
    }
    if (on) {
      this.typingTimer = setTimeout(() => this.setTyping(false), TYPING_RESET_MS);
      this.typingTimer.unref();
    }
    if (this.typing === on) {
      return;
    }
    this.typing = on;
    if (this.rl && this.outputError === null) {
      // readline still has the old prompt here, so the rows of the drawn line are cleared correctly.
      this.clearEditArea(this.rl, this.output!);
      this.redrawPrompt(this.rl);
    }
  }

  private async body(message: AdapterPostableMessage): Promise<string[]> {
    const lines: string[] = [];
    const card = extractCard(message);
    let text: string | null = null;
    if (typeof message === "string") {
      text = message;
    } else if ("raw" in message) {
      text = message.raw;
    } else if ("markdown" in message) {
      text = message.markdown;
    } else if ("ast" in message) {
      text = stringifyMarkdown(message.ast);
    } else if ("card" in message && message.fallbackText) {
      text = message.fallbackText;
    }
    if (text !== null && text !== "") {
      lines.push(convertEmojiPlaceholders(text, "gchat"));
    }
    if (card) {
      lines.push(indent(JSON.stringify(card as CardElement, null, 2)));
    }
    return lines;
  }

  private async saveFiles(id: string, message: AdapterPostableMessage): Promise<string[]> {
    const lines: string[] = [];
    const uploads: FileUpload[] = extractFiles(message);
    const attachments = extractPostableAttachments(message);
    const items: { name: string; data: Buffer | null; mimeType?: string; url?: string }[] = [];
    for (const f of uploads) {
      items.push({ name: f.filename, data: await toBuffer(f.data, SHARED), mimeType: f.mimeType });
    }
    for (const a of attachments) {
      const data = a.data ? await toBuffer(a.data, SHARED) : a.fetchData ? await toBuffer(await a.fetchData(), SHARED) : null;
      items.push({ name: a.name ?? a.type, data, mimeType: a.mimeType, url: a.url });
    }
    if (items.length === 0) {
      return lines;
    }
    const dir = join(this.options.filesDir, localDate(new Date()));
    await mkdir(dir, { recursive: true, mode: 0o700 });
    for (const item of items) {
      const mimeType = item.mimeType ?? mime.getType(item.name) ?? "未知类型";
      if (!item.data) {
        lines.push(`  [文件] ${item.name}，${mimeType} → ${item.url ?? "没有内容"}`);
        continue;
      }
      const path = join(dir, `${id}-${safeFileName(item.name)}`);
      await writeFile(path, item.data, { mode: 0o600 });
      lines.push(`  [文件] ${item.name}，${item.data.length} 字节，${mimeType} → ${path}`);
    }
    return lines;
  }

  // ---- Adapter ----

  handleWebhook(_request: Request, _options?: WebhookOptions): Promise<Response> {
    throw new NotImplementedError("the console adapter has no webhooks", "webhook");
  }

  encodeThreadId(_platformData: Record<string, never>): string {
    return CONSOLE_CHANNEL;
  }

  decodeThreadId(threadId: string): Record<string, never> {
    if (threadId !== CONSOLE_CHANNEL) {
      throw new Error(`not a console thread id: ${threadId}`);
    }
    return {};
  }

  channelIdFromThreadId(threadId: string): string {
    this.decodeThreadId(threadId);
    return CONSOLE_CHANNEL;
  }

  isDM(_threadId: string): boolean {
    return true;
  }

  parseMessage(raw: ConsoleRaw): Message<ConsoleRaw> {
    const attachments: Attachment[] = [];
    if (raw.file) {
      const file = raw.file;
      attachments.push({
        type: attachmentType(file.mimeType),
        name: file.name,
        size: file.size,
        mimeType: file.mimeType,
        fetchData: () => readFile(file.path),
      });
    }
    return new Message<ConsoleRaw>({
      id: raw.id,
      threadId: CONSOLE_CHANNEL,
      text: raw.text,
      formatted: parseMarkdown(raw.text),
      raw,
      author: OPERATOR,
      metadata: { dateSent: new Date(raw.time), edited: false },
      attachments,
      isMention: true,
    });
  }

  renderFormatted(content: FormattedContent): string {
    return stringifyMarkdown(content);
  }

  async postMessage(threadId: string, message: AdapterPostableMessage): Promise<RawMessage<ConsoleRaw>> {
    this.decodeThreadId(threadId);
    const id = this.nextId();
    const now = new Date();
    const lines = [`[${clock(now)} ${this.userName} #${seqPart(id)}]`, ...(await this.body(message))];
    lines.push(...(await this.saveFiles(id, message)));
    this.setTyping(false);
    await this.sendBlock(lines.join("\n"));
    return { id, threadId, raw: { id, text: lines.slice(1).join("\n"), time: now.getTime() } };
  }

  async editMessage(threadId: string, messageId: string, message: AdapterPostableMessage): Promise<RawMessage<ConsoleRaw>> {
    this.decodeThreadId(threadId);
    const now = new Date();
    const lines = [`[${clock(now)} ${this.userName} #${seqPart(messageId)} 已编辑]`, ...(await this.body(message))];
    await this.sendBlock(lines.join("\n"));
    return { id: messageId, threadId, raw: { id: messageId, text: lines.slice(1).join("\n"), time: now.getTime() } };
  }

  async deleteMessage(threadId: string, messageId: string): Promise<void> {
    this.decodeThreadId(threadId);
    await this.sendBlock(`[${clock(new Date())} ${this.userName} #${seqPart(messageId)} 已删除]`);
  }

  async addReaction(threadId: string, messageId: string, emoji: EmojiValue | string): Promise<void> {
    this.decodeThreadId(threadId);
    await this.sendBlock(`[${clock(new Date())} ${this.userName} #${seqPart(messageId)} +${emojiText(emoji)}]`);
  }

  async removeReaction(threadId: string, messageId: string, emoji: EmojiValue | string): Promise<void> {
    this.decodeThreadId(threadId);
    await this.sendBlock(`[${clock(new Date())} ${this.userName} #${seqPart(messageId)} -${emojiText(emoji)}]`);
  }

  async startTyping(threadId: string, _status?: string): Promise<void> {
    this.decodeThreadId(threadId);
    if (this.interactive) {
      this.setTyping(true);
    }
  }

  async fetchMessages(_threadId: string, _options?: FetchOptions): Promise<FetchResult<ConsoleRaw>> {
    // History comes from message_log; an empty page lets chat-sdk fall back to its own cache (contracts 9.4).
    return { messages: [] };
  }

  async fetchThread(_threadId: string): Promise<ThreadInfo> {
    throw new NotImplementedError("the console adapter cannot fetch thread information", "fetchThread");
  }
}

export function createConsoleAdapter(options: ConsoleAdapterOptions): ConsoleAdapter {
  return new ConsoleAdapter(options);
}
