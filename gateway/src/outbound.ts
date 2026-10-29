import { closeSync, constants, readFileSync, realpathSync } from "node:fs";
import { basename, isAbsolute, resolve, sep } from "node:path";
import { QQAdapter } from "@amatsuka/chat-adapter-qq";
import { TelegramAdapter, TelegramFormatConverter } from "@chat-adapter/telegram";
import type { Adapter, AdapterPostableMessage, CardElement, FileUpload, RawMessage } from "chat";
import mime from "mime";
import type { PinoLogger } from "./log.ts";
import type { MessageLog } from "./message-log.ts";
import { GatewayQQAdapter, type Platforms } from "./platforms.ts";
import type { Repo } from "./repo.ts";
import { openNoFollow, type OpenedFile } from "./safe-open.ts";
import type { AttachmentType, OutboundPayload, WireAttachment } from "./types.ts";

export const TELEGRAM_TEXT_LIMIT = 4096;
export const TELEGRAM_CAPTION_LIMIT = 1024;
const TELEGRAM_MEDIA_GROUP_MAX = 10;

/** An error whose message is shown to the caller (bearbot-im or the chat user). */
export class OutboundError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "OutboundError";
  }
}

export type LoadedFile = { data: Buffer; filename: string; mimeType?: string };

export type PostContent = {
  /** Markdown from bearbot-im. */
  markdown?: string;
  /** Plain text written by the gateway itself (command replies, notices). */
  text?: string;
  card?: CardElement;
  files?: LoadedFile[];
  /** Platform message id to reply to (Telegram and QQ; the console adapter has no reply call). */
  replyTo?: string;
};

/** The part of a QQ send response that the adapter keeps in `raw` but does not type. */
type QQSendResponse = { ext_info?: { ref_idx?: string } };

type Part = { message: AdapterPostableMessage; text: string; files: LoadedFile[]; card?: CardElement };

/** QQ with `[platforms.qq] markdown = false`: native markdown messages need a platform permission, so text goes out as plain content. */
function qqPlain(adapter: Adapter): boolean {
  return adapter instanceof GatewayQQAdapter && !adapter.markdown;
}

export function attachmentTypeOf(mimeType: string | undefined): AttachmentType {
  if (mimeType?.startsWith("image/")) return "image";
  if (mimeType?.startsWith("audio/")) return "audio";
  if (mimeType?.startsWith("video/")) return "video";
  return "file";
}

function fenceMarker(line: string): string | null {
  const m = /^\s*(`{3,}|~{3,})/.exec(line);
  return m ? m[1]! : null;
}

/** Longest prefix (whole code points) of `text` accepted by `fits`; at least one code point. */
function longestPrefix(text: string, fits: (s: string) => boolean): string {
  const cps = Array.from(text);
  let lo = 1;
  let hi = cps.length;
  if (!fits(cps[0]!)) {
    throw new OutboundError("文本中的单个字符超过平台长度限制");
  }
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (fits(cps.slice(0, mid).join(""))) {
      lo = mid;
    } else {
      hi = mid - 1;
    }
  }
  return cps.slice(0, lo).join("");
}

/**
 * Splits `text` into parts with `measure(part) <= limit`, at line ends where possible.
 * In markdown mode a fenced code block cut between parts is closed and reopened.
 */
export function splitText(text: string, limit: number, measure: (s: string) => number, markdown: boolean): string[] {
  if (measure(text) <= limit) {
    return [text];
  }
  const parts: string[] = [];
  const lines = text.split("\n");
  let cur: string[] = [];
  let startFence: string | null = null;
  let openFence: string | null = null;

  const render = (ls: string[], fenceAtEnd: string | null): string => {
    const body = (startFence !== null ? [startFence, ...ls] : ls).join("\n");
    return fenceAtEnd !== null ? `${body}\n${fenceMarker(fenceAtEnd)}` : body;
  };
  const fenceAfter = (fence: string | null, line: string): string | null => {
    if (!markdown) return null;
    const marker = fenceMarker(line);
    if (marker === null) return fence;
    if (fence === null) return line.trim();
    const open = fenceMarker(fence)!;
    const closes = line.trim() === marker && marker[0] === open[0] && marker.length >= open.length;
    return closes ? null : fence;
  };
  const flush = (): void => {
    const part = render(cur, openFence).trimEnd();
    if (part.trim() !== "") {
      parts.push(part);
    }
    startFence = openFence;
    cur = [];
  };

  for (const line of lines) {
    const nextFence = fenceAfter(openFence, line);
    if (measure(render([...cur, line], nextFence)) <= limit) {
      cur.push(line);
      openFence = nextFence;
      continue;
    }
    if (cur.length > 0) {
      flush();
      if (measure(render([line], nextFence)) <= limit) {
        cur.push(line);
        openFence = nextFence;
        continue;
      }
    }
    // One line longer than the limit: cut it.
    let rest = line;
    while (measure(render([rest], nextFence)) > limit) {
      const head = longestPrefix(rest, (p) => measure(render([p], openFence)) <= limit);
      cur.push(head);
      flush();
      rest = rest.slice(head.length);
    }
    cur.push(rest);
    openFence = nextFence;
  }
  if (cur.length > 0) {
    flush();
  }
  return parts;
}

function chunk<T>(items: T[], size: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += size) {
    out.push(items.slice(i, i + size));
  }
  return out;
}

export function parseRef(ref: string): { threadId: string; messageId: string } {
  const i = ref.indexOf("|");
  if (i <= 0 || i === ref.length - 1) {
    throw new OutboundError(`消息引用格式不正确：${ref}`);
  }
  return { threadId: ref.slice(0, i), messageId: ref.slice(i + 1) };
}

/** Sending, splitting and file reading (DESIGN 6.8, contracts 9.4). Every sent message is logged. */
export class Outbound {
  private inflight = 0;
  private idleWaiters: (() => void)[] = [];
  private readonly telegramConverter = new TelegramFormatConverter();

  constructor(
    private readonly platforms: Platforms,
    private readonly log: MessageLog,
    private readonly repo: Repo,
    private readonly logger: PinoLogger,
    private readonly workspaceDir: string,
  ) {}

  /** Resolves when no send is running (SIGTERM waits for it). */
  whenIdle(): Promise<void> {
    if (this.inflight === 0) {
      return Promise.resolve();
    }
    return new Promise((r) => this.idleWaiters.push(r));
  }

  private async track<T>(fn: () => Promise<T>): Promise<T> {
    this.inflight += 1;
    try {
      return await fn();
    } finally {
      this.inflight -= 1;
      if (this.inflight === 0) {
        const waiters = this.idleWaiters;
        this.idleWaiters = [];
        for (const w of waiters) w();
      }
    }
  }

  adapterOf(id: string): Adapter {
    const adapter = this.platforms.adapterFor(id);
    if (!adapter) {
      throw new OutboundError(`平台没有启用：${id}`);
    }
    return adapter;
  }

  /** Reads a file for sending; the real path must be inside the workspace directory (DESIGN 6.8). */
  loadWorkspaceFile(path: string, name?: string): LoadedFile {
    const workspace = realpathSync(this.workspaceDir);
    const absolute = isAbsolute(path) ? path : resolve(workspace, path);
    let real: string;
    try {
      real = realpathSync(absolute);
    } catch (e) {
      throw new OutboundError(`无法读取文件 ${path}：${(e as Error).message}`);
    }
    const inWorkspace = (p: string): boolean => p === workspace || p.startsWith(workspace + sep);
    if (!inWorkspace(real)) {
      throw new OutboundError(`文件不在工作区目录内：${path}`);
    }
    // The agent can swap a directory of `real` for a symlink after the check above, so the opened file is checked again.
    let opened: OpenedFile;
    try {
      opened = openNoFollow(real, constants.O_RDONLY);
    } catch (e) {
      throw new OutboundError(`无法读取文件 ${path}：${(e as Error).message}`);
    }
    try {
      if (!inWorkspace(opened.path)) {
        throw new OutboundError(`文件不在工作区目录内：${path}`);
      }
      if (!opened.stats.isFile()) {
        throw new OutboundError(`不是普通文件：${path}`);
      }
      const filename = name && name !== "" ? name : basename(absolute);
      return { data: readFileSync(opened.fd), filename, mimeType: mime.getType(filename) ?? undefined };
    } finally {
      closeSync(opened.fd);
    }
  }

  private telegramParts(text: string, plain: boolean, files: LoadedFile[]): Part[] {
    const measure = plain ? (s: string) => s.length : (s: string) => this.telegramConverter.fromMarkdown(s).length;
    const make = (t: string, f: LoadedFile[]): Part => ({
      message: plain ? { raw: t, files: f.map(toUpload) } : { markdown: t, files: f.map(toUpload) },
      text: t,
      files: f,
    });
    const parts: Part[] = [];
    const batches = chunk(files, TELEGRAM_MEDIA_GROUP_MAX);
    if (batches.length > 0) {
      if (text === "" || measure(text) <= TELEGRAM_CAPTION_LIMIT) {
        batches.forEach((b, i) => parts.push(make(i === 0 ? text : "", b)));
      } else {
        for (const t of splitText(text, TELEGRAM_TEXT_LIMIT, measure, !plain)) parts.push(make(t, []));
        for (const b of batches) parts.push(make("", b));
      }
    } else if (text !== "") {
      for (const t of splitText(text, TELEGRAM_TEXT_LIMIT, measure, !plain)) parts.push(make(t, []));
    }
    return parts;
  }

  private parts(adapter: Adapter, content: PostContent): Part[] {
    const plain = content.markdown === undefined;
    const text = content.markdown ?? content.text ?? "";
    const files = content.files ?? [];
    let parts: Part[];
    if (adapter.name === "telegram") {
      parts = this.telegramParts(text, plain, files);
    } else if (text !== "" || files.length > 0) {
      const message: AdapterPostableMessage =
        plain || qqPlain(adapter) ? { raw: text, files: files.map(toUpload) } : { markdown: text, files: files.map(toUpload) };
      parts = [{ message, text, files }];
    } else {
      parts = [];
    }
    if (content.card) {
      parts.push({ message: { card: content.card }, text: "", files: [], card: content.card });
    }
    return parts;
  }

  /** Sends to a thread or channel; returns the ref "<thread>|<message id>" of the first part sent. */
  post(target: string, content: PostContent): Promise<string> {
    return this.track(async () => {
      const adapter = this.adapterOf(target);
      const parts = this.parts(adapter, content);
      if (parts.length === 0) {
        throw new OutboundError("没有要发送的内容");
      }
      const channelId = adapter.channelIdFromThreadId(target);
      const channel = {
        id: channelId,
        name: this.repo.binding(channelId)?.channelName ?? undefined,
        isDM: adapter.isDM?.(target) ?? false,
      };
      // QQ quotes a message by its quote index, recorded in message_log when it was received or sent (DESIGN 6.8).
      let quote: string | null = null;
      if (content.replyTo && adapter instanceof QQAdapter) {
        const row = this.log.findMessage(target, content.replyTo);
        if (!row) {
          throw new OutboundError(`找不到要回复的消息：${content.replyTo}`);
        }
        quote = row.quoteId;
        if (quote === null) {
          this.logger.info({ target, replyTo: content.replyTo }, "QQ gave no quote index for the replied-to message, sent without a quote");
        }
        // QQ clients show the text of a quoting markdown message twice; a card is sent as markdown and has no plain form.
        if (quote !== null && parts[0]!.card) {
          throw new OutboundError("QQ 不能引用回复卡片消息");
        }
      }
      let first: string | null = null;
      for (const [i, part] of parts.entries()) {
        let sent: RawMessage<unknown>;
        if (i === 0 && content.replyTo && adapter instanceof TelegramAdapter) {
          sent = await adapter.postMessage(target, part.message, content.replyTo);
        } else if (i === 0 && quote !== null && adapter instanceof QQAdapter) {
          // Like Telegram's allow_sending_without_reply: a quoted message QQ no longer finds does not stop the send.
          // The quoting message is plain text: QQ clients show the text of a quoting markdown message twice.
          sent = await adapter.postQQMessage(target, { raw: part.text, files: part.files.map(toUpload) }, {
            messageReference: { message_id: quote, ignore_get_message_error: true },
          });
        } else {
          sent = await adapter.postMessage(target, part.message);
        }
        const threadId = sent.threadId || target;
        const payload: OutboundPayload = {
          platform: adapter.name,
          channel,
          text: part.text,
          attachments: part.files.map(
            (f): WireAttachment => ({ type: attachmentTypeOf(f.mimeType), name: f.filename, size: f.data.length, mimeType: f.mimeType }),
          ),
        };
        if (part.card) {
          payload.card = part.card;
        }
        const quoteId = adapter instanceof QQAdapter ? ((sent.raw as QQSendResponse).ext_info?.ref_idx ?? null) : null;
        this.log.insertOutbound({ threadId, channelId, messageId: sent.id, payload, quoteId });
        first ??= `${threadId}|${sent.id}`;
      }
      return first!;
    });
  }

  /** Plain text from the gateway; failures are logged (used where nobody waits for the result). */
  postNotice(target: string, text: string): void {
    this.post(target, { text }).catch((e: unknown) => {
      this.logger.error({ target, err: e }, "sending a notice failed");
    });
  }

  edit(ref: string, content: { markdown?: string; card?: CardElement }): Promise<void> {
    return this.track(async () => {
      const { threadId, messageId } = parseRef(ref);
      const adapter = this.adapterOf(threadId);
      if (content.card) {
        await adapter.editMessage(threadId, messageId, { card: content.card });
      } else if (content.markdown !== undefined) {
        await adapter.editMessage(
          threadId,
          messageId,
          qqPlain(adapter) ? { raw: content.markdown } : { markdown: content.markdown },
        );
      } else {
        throw new OutboundError("edit 需要 markdown 或 card");
      }
    });
  }

  delete(ref: string): Promise<void> {
    return this.track(async () => {
      const { threadId, messageId } = parseRef(ref);
      await this.adapterOf(threadId).deleteMessage(threadId, messageId);
    });
  }

  react(ref: string, emoji: string, remove: boolean): Promise<void> {
    return this.track(async () => {
      const { threadId, messageId } = parseRef(ref);
      const adapter = this.adapterOf(threadId);
      if (remove) {
        await adapter.removeReaction(threadId, messageId, emoji);
      } else {
        await adapter.addReaction(threadId, messageId, emoji);
      }
    });
  }

  typing(target: string): Promise<void> {
    return this.track(async () => {
      await this.adapterOf(target).startTyping(target);
    });
  }
}

function toUpload(f: LoadedFile): FileUpload {
  return f.mimeType ? { data: f.data, filename: f.filename, mimeType: f.mimeType } : { data: f.data, filename: f.filename };
}
