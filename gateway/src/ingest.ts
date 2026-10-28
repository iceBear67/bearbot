import { randomBytes } from "node:crypto";
import { closeSync, constants, realpathSync, writeFile, type Stats } from "node:fs";
import { lstat, mkdir, rename } from "node:fs/promises";
import { join } from "node:path";
import { promisify } from "node:util";
import { downloadAttachment, toBuffer, type PlatformName } from "@chat-adapter/shared";
import type { TelegramChat, TelegramMessage } from "@chat-adapter/telegram";
import type { QQRawMessage } from "@amatsuka/chat-adapter-qq";
import type { ActionEvent, Adapter, Attachment, Author, Message, ReactionEvent, SlashCommandEvent, Thread } from "chat";
import mime from "mime";
import type { AgentState } from "./agent-state.ts";
import type { CommandContext, Commands } from "./commands.ts";
import type { GatewayConfig } from "./config.ts";
import { safeFileName, type ConsoleRaw } from "./console-adapter.ts";
import { errorText, type PinoLogger } from "./log.ts";
import type { MessageLog } from "./message-log.ts";
import type { Outbound } from "./outbound.ts";
import { CONSOLE_CHANNEL_NAME, CONSOLE_OPERATOR, type BindingRow, type Repo } from "./repo.ts";
import { openNoFollow } from "./safe-open.ts";
import { isoLocal, localDate } from "./time.ts";
import { TOKEN_LIKE, type Tokens } from "./tokens.ts";
import type { InboundPayload, MessageKind, Role, WireAttachment, WireMessage } from "./types.ts";

/** Sends finished message_log rows to bearbot-im (implemented by the bridge server). */
export interface Delivery {
  pump(): void;
  adminsChanged(): void;
}

class DownloadTimeout extends Error {}

function identityOf(platform: string, author: Author): string {
  return `${platform}:${author.userId}`;
}

function nameOf(author: Author): string {
  return author.fullName || author.userName || author.userId;
}

type Sender = { identity: string; name: string; isBot: boolean };

/** Identity and name of a Telegram chat that sent a message on its own behalf. */
function chatSender(chat: TelegramChat): { identity: string; name: string } {
  return { identity: `telegram:chat:${chat.id}`, name: chat.title ?? chat.username ?? String(chat.id) };
}

/**
 * Sender of a message or command. A Telegram message sent on behalf of a chat (anonymous group admin, post as a
 * channel, automatic forward from a linked channel) carries a fake `from` user that is the same for every chat,
 * so the chat in `sender_chat` is the sender: `telegram:chat:<id>` (DESIGN 1, 15).
 */
function senderOf(platform: string, author: Author, raw: unknown): Sender {
  if (platform === "telegram") {
    const chat = (raw as TelegramMessage).sender_chat;
    if (chat) {
      return { ...chatSender(chat), isBot: false };
    }
  }
  return { identity: identityOf(platform, author), name: nameOf(author), isBot: author.isBot === true };
}

/** In forum topics Telegram sets reply_to_message to the topic root for messages that are not explicit replies. */
function isTopicRootReply(raw: TelegramMessage): boolean {
  return raw.is_topic_message === true && raw.reply_to_message?.message_id === raw.message_thread_id;
}

/** Leading mention markup of QQ group messages, removed the same way the adapter does for commands. */
function stripQQMentions(text: string): string {
  let out = text.trimStart();
  let previous: string;
  do {
    previous = out;
    out = out
      .replace(/^<@!?[^>\s]+>\s*/u, "")
      .replace(/^<qqbot-at-user\s+id=(?:"[^"]+"|'[^']+')[^>]*\/>\s*/u, "")
      .replace(/^@\S+\s+/u, "")
      .trimStart();
  } while (out !== previous);
  return out;
}

/** Untouched message text for token and /bb detection (chat-platforms.md 5.1). */
function rawTextOf(platform: string, message: Message): string {
  if (platform === "telegram") {
    const raw = message.raw as TelegramMessage;
    return raw.text ?? raw.caption ?? "";
  }
  if (platform === "qq") {
    const raw = message.raw as QQRawMessage;
    const content = raw.content ?? "";
    return raw._chat_is_mention === true ? stripQQMentions(content) : content;
  }
  return message.text;
}

export function channelNameOf(platform: string, raw: unknown): string | null {
  if (platform === "telegram") {
    const chat = (raw as TelegramMessage).chat;
    const personal = [chat.first_name, chat.last_name].filter(Boolean).join(" ");
    return chat.title ?? (personal !== "" ? personal : (chat.username ?? null));
  }
  if (platform === "console") {
    return CONSOLE_CHANNEL_NAME;
  }
  return null;
}

function replyToOf(platform: string, message: Message): WireMessage["replyTo"] {
  if (platform === "qq") {
    const quoted = (message.raw as QQRawMessage)._chat_quoted_message;
    return quoted ? { id: quoted.msgIdx, sender: "", name: "", text: quoted.content ?? "" } : undefined;
  }
  if (platform === "telegram" && isTopicRootReply(message.raw as TelegramMessage)) {
    return undefined;
  }
  const r = message.replyTo;
  if (!r) {
    return undefined;
  }
  const sender = senderOf(platform, r.author, r.raw);
  return { id: r.id, sender: sender.identity, name: sender.name, text: r.text };
}

/** Author of the replied-to message, for reply-based `/bb admin add`. */
function replyTargetOf(platform: string, raw: unknown, replyTo: Message | undefined): CommandContext["replyTarget"] {
  if (platform === "telegram") {
    const message = raw as TelegramMessage;
    if (isTopicRootReply(message)) {
      return null;
    }
    const chat = message.reply_to_message?.sender_chat;
    if (chat) {
      return chatSender(chat);
    }
    const from = message.reply_to_message?.from;
    return from ? { identity: `telegram:${from.id}`, name: [from.first_name, from.last_name].filter(Boolean).join(" ") || null } : null;
  }
  if (replyTo) {
    return { identity: identityOf(platform, replyTo.author), name: nameOf(replyTo.author) };
  }
  return null;
}

/** "/bb", "/bb@name" followed by arguments. */
const BB = /^\/bb(?:@(\S+))?(?:\s([\s\S]*))?$/i;

function defaultAttachmentName(a: Attachment, index: number): string {
  const ext = a.mimeType ? mime.getExtension(a.mimeType) : null;
  return `${a.type}-${index + 1}${ext ? `.${ext}` : ""}`;
}

const writeFd = promisify(writeFile);

/** Creates `path` when it is missing; it must be a real directory, a symlink is refused. */
async function realDirectory(path: string): Promise<void> {
  let st: Stats;
  try {
    st = await lstat(path);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== "ENOENT") {
      throw e;
    }
    try {
      await mkdir(path, { mode: 0o775 });
    } catch (e2) {
      // Created in the meantime by another download or by the agent; checked below like an existing entry.
      if ((e2 as NodeJS.ErrnoException).code !== "EEXIST") {
        throw e2;
      }
    }
    st = await lstat(path);
  }
  if (!st.isDirectory()) {
    throw new Error(`${path} 不是目录，附件没有保存`);
  }
}

/**
 * Saves an attachment as bearbot-gw into the inbox, which the agent can write (DESIGN 15). The inbox directories
 * must be real directories, the data goes into a new temporary file checked after opening, and rename(2) puts it
 * at the final name: an earlier file or a symlink there is replaced without being followed, so an edited message
 * still replaces its earlier file.
 */
async function saveInboxFile(workspaceDir: string, date: string, fileName: string, data: Buffer): Promise<void> {
  const inbox = join(realpathSync(workspaceDir), "inbox");
  const dir = join(inbox, date);
  await realDirectory(inbox);
  await realDirectory(dir);
  const tmp = join(dir, `.${fileName}.${randomBytes(8).toString("hex")}.tmp`);
  const opened = openNoFollow(tmp, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL, 0o664);
  try {
    if (opened.path !== tmp || !opened.stats.isFile()) {
      throw new Error(`临时文件 ${tmp} 不在 inbox 目录中（实际为 ${opened.path}），附件没有保存`);
    }
    await writeFd(opened.fd, data);
  } finally {
    closeSync(opened.fd);
  }
  await rename(tmp, join(dir, fileName));
}

/** Inbound processing (DESIGN 6.5). Handlers only write message_log and return. */
export class Ingest {
  constructor(
    private readonly deps: {
      cfg: GatewayConfig;
      repo: Repo;
      log: MessageLog;
      tokens: Tokens;
      commands: Commands;
      outbound: Outbound;
      agentState: AgentState;
      delivery: Delivery;
      logger: PinoLogger;
    },
  ) {}

  private roleOf(identity: string): Role {
    return this.deps.repo.isAdmin(identity) ? "admin" : "member";
  }

  /** onDirectMessage / onNewMention / onNewMessage. */
  async message(thread: Thread, message: Message): Promise<void> {
    const adapter = thread.adapter;
    const platform = adapter.name;
    const sender = senderOf(platform, message.author, message.raw);
    const identity = sender.identity;
    const rawText = rawTextOf(platform, message).trim();

    // 1. admin token
    if (TOKEN_LIKE.test(rawText)) {
      this.claimToken(rawText, identity, sender.name, thread.id);
      return;
    }

    // 2. /bb in plain text (for example "/bb@other_bot", or an edited command)
    const bb = BB.exec(rawText);
    if (bb) {
      if (bb[1] !== undefined && bb[1].toLowerCase() !== adapter.userName.toLowerCase()) {
        return; // addressed to another bot
      }
      this.runCommand({
        adapter,
        platform,
        threadId: thread.id,
        channelId: thread.channelId,
        isDM: thread.isDM,
        sender: { identity, name: sender.name },
        args: (bb[2] ?? "").trim(),
        channelName: channelNameOf(platform, message.raw),
        replyTarget: replyTargetOf(platform, message.raw, message.replyTo),
      });
      return;
    }

    // 3. binding
    const binding = this.deps.repo.binding(thread.channelId);
    if (!binding) {
      return;
    }

    // 4. role, 5. trigger
    const role = this.roleOf(identity);
    const edited = message.metadata.edited;
    let text = message.text;
    let trigger: boolean;
    switch (binding.trigger) {
      case "mention":
        trigger = message.isMention === true || thread.isDM;
        break;
      case "all":
        trigger = true;
        break;
      case "prefix": {
        const body = text.trimStart();
        trigger = binding.prefix !== null && body.startsWith(binding.prefix);
        if (trigger) {
          text = body.slice(binding.prefix!.length).trimStart();
        }
        break;
      }
    }
    if (edited) {
      trigger = false;
    }

    const payload: InboundPayload = {
      kind: "message",
      id: message.id,
      platform,
      channel: this.channelInfo(binding, thread.isDM, channelNameOf(platform, message.raw)),
      thread: thread.id,
      sender: { id: identity, name: sender.name, role, isBot: sender.isBot },
      time: isoLocal(message.metadata.dateSent),
      text,
      mentionsBot: message.isMention === true,
      edited,
      attachments: [],
      trigger,
    };
    const replyTo = replyToOf(platform, message);
    if (replyTo) {
      payload.replyTo = replyTo;
    }
    this.record("message", payload, binding.channelId, message.attachments);
  }

  /** onSlashCommand: thread = event.channel.id (contracts 9.4). */
  async command(event: SlashCommandEvent): Promise<void> {
    const adapter = event.adapter;
    const platform = adapter.name;
    const threadId = event.channel.id;
    const channelId = adapter.channelIdFromThreadId(threadId);
    const isDM = adapter.isDM?.(threadId) ?? false;
    const sender = senderOf(platform, event.user, event.raw);
    const identity = sender.identity;
    const args = this.commandArgs(event);

    if (event.command.toLowerCase() === "/bb") {
      const parsed = this.parseRaw(adapter, event.raw);
      this.runCommand({
        adapter,
        platform,
        threadId,
        channelId,
        isDM,
        sender: { identity, name: sender.name },
        args,
        channelName: channelNameOf(platform, event.raw),
        replyTarget: replyTargetOf(platform, event.raw, parsed?.replyTo),
      });
      return;
    }

    const binding = this.deps.repo.binding(channelId);
    if (!binding) {
      return;
    }
    const parsed = this.parseRaw(adapter, event.raw);
    const payload: InboundPayload = {
      kind: "command",
      id: parsed?.id ?? event.triggerId ?? "",
      platform,
      channel: this.channelInfo(binding, isDM, channelNameOf(platform, event.raw)),
      thread: threadId,
      sender: { id: identity, name: sender.name, role: this.roleOf(identity), isBot: sender.isBot },
      time: isoLocal(parsed ? parsed.metadata.dateSent : new Date()),
      text: args === "" ? event.command : `${event.command} ${args}`,
      mentionsBot: true,
      edited: false,
      command: { name: event.command, args },
      attachments: [],
      trigger: true,
    };
    const replyTo = parsed ? replyToOf(platform, parsed) : undefined;
    if (replyTo) {
      payload.replyTo = replyTo;
    }
    this.record("command", payload, channelId, parsed?.attachments ?? []);
  }

  /** onAction: kind "action", trigger false. */
  async action(event: ActionEvent): Promise<void> {
    if (!event.threadId) {
      return;
    }
    const adapter = event.adapter;
    const channelId = adapter.channelIdFromThreadId(event.threadId);
    const binding = this.deps.repo.binding(channelId);
    if (!binding) {
      return;
    }
    const identity = identityOf(adapter.name, event.user);
    const action: NonNullable<WireMessage["action"]> = { actionId: event.actionId };
    if (event.value !== undefined) {
      action.value = event.value;
    }
    this.record(
      "action",
      {
        kind: "action",
        id: event.messageId,
        platform: adapter.name,
        channel: this.channelInfo(binding, adapter.isDM?.(event.threadId) ?? false, null),
        thread: event.threadId,
        sender: { id: identity, name: nameOf(event.user), role: this.roleOf(identity), isBot: event.user.isBot === true },
        time: isoLocal(new Date()),
        text: event.value ?? "",
        mentionsBot: false,
        edited: false,
        action,
        attachments: [],
        trigger: false,
      },
      channelId,
      [],
    );
  }

  /** onReaction: kind "reaction", trigger false. */
  async reaction(event: ReactionEvent): Promise<void> {
    const adapter = event.adapter;
    const channelId = adapter.channelIdFromThreadId(event.threadId);
    const binding = this.deps.repo.binding(channelId);
    if (!binding) {
      return;
    }
    const identity = identityOf(adapter.name, event.user);
    const emoji = event.rawEmoji !== "" ? event.rawEmoji : event.emoji.name;
    this.record(
      "reaction",
      {
        kind: "reaction",
        id: event.messageId,
        platform: adapter.name,
        channel: this.channelInfo(binding, adapter.isDM?.(event.threadId) ?? false, null),
        thread: event.threadId,
        sender: { id: identity, name: nameOf(event.user), role: this.roleOf(identity), isBot: event.user.isBot === true },
        time: isoLocal(new Date()),
        text: "",
        mentionsBot: false,
        edited: false,
        reaction: { emoji, added: event.added, messageId: event.messageId },
        attachments: [],
        trigger: false,
      },
      channelId,
      [],
    );
  }

  /** Command arguments with original spacing: QQ strips markdown from event.text, so its raw content is used. */
  private commandArgs(event: SlashCommandEvent): string {
    if (event.adapter.name === "qq") {
      const raw = event.raw as QQRawMessage;
      const content = raw._chat_is_mention === true ? stripQQMentions(raw.content ?? "") : (raw.content ?? "").trimStart();
      const m = /^\/\S+(?:\s([\s\S]*))?$/.exec(content.trim());
      return (m?.[1] ?? "").trim();
    }
    return event.text.trim();
  }

  private parseRaw(adapter: Adapter, raw: unknown): Message | null {
    if (raw === undefined || raw === null) {
      return null;
    }
    if (adapter.name === "console") {
      return adapter.parseMessage(raw as ConsoleRaw);
    }
    return adapter.parseMessage(raw);
  }

  private channelInfo(binding: BindingRow, isDM: boolean, platformName: string | null): WireMessage["channel"] {
    const name = platformName ?? binding.channelName;
    return name !== null ? { id: binding.channelId, name, isDM } : { id: binding.channelId, isDM };
  }

  private runCommand(ctx: CommandContext): void {
    // Not awaited: commands may wait for the daemon (restart, repair), polling must go on.
    void this.deps.commands.run(ctx);
  }

  private claimToken(token: string, identity: string, displayName: string, threadId: string): void {
    const { repo, tokens, outbound, delivery } = this.deps;
    if (identity === CONSOLE_OPERATOR) {
      outbound.postNotice(threadId, "控制台用户是内置管理员，不需要领取令牌");
      return;
    }
    const result = tokens.claim(token, { identity, displayName, thread: threadId });
    if (result === "claimed") {
      this.deps.logger.info({ identity, thread: threadId }, "admin token claimed");
      delivery.adminsChanged();
      outbound.postNotice(threadId, "已成为 bearbot 管理员");
    } else {
      this.deps.logger.warn({ identity, thread: threadId, admin: repo.isAdmin(identity) }, "invalid admin token");
      outbound.postNotice(threadId, "令牌无效、已经使用或已经过期");
    }
  }

  /** Steps 6-8: insert in a transaction, download attachments in the background, deliver, maintenance notice. */
  private record(kind: MessageKind, payload: InboundPayload, channelId: string, sources: Attachment[]): void {
    const { log, delivery, agentState, outbound, cfg } = this.deps;
    const names = new Set<string>();
    payload.attachments = sources.map((a, i): WireAttachment => {
      let name = a.name && a.name !== "" ? a.name : defaultAttachmentName(a, i);
      if (names.has(name)) {
        name = `${i + 1}-${name}`;
      }
      names.add(name);
      const att: WireAttachment = { type: a.type, name };
      if (a.size !== undefined) att.size = a.size;
      if (a.mimeType !== undefined) att.mimeType = a.mimeType;
      return att;
    });
    if (sources.length > 0) {
      payload.pending = true;
    }
    const seq = log.insertInbound({
      kind,
      threadId: payload.thread,
      channelId,
      messageId: payload.id,
      payload,
      createdAt: Date.now(),
    });
    if (sources.length > 0) {
      void this.download(seq, payload, sources);
    } else {
      delivery.pump();
    }
    if (payload.trigger && agentState.takeNotice(channelId)) {
      outbound.postNotice(payload.thread, cfg.maintenanceNotice);
    }
  }

  /** Step 7: attachments one by one into <workspace>/inbox/<date>/<msgid>-<name>. */
  private async download(seq: number, payload: InboundPayload, sources: Attachment[]): Promise<void> {
    const { cfg, log, delivery } = this.deps;
    const date = localDate(new Date());
    for (const [i, source] of sources.entries()) {
      const att = payload.attachments[i]!;
      const fileName = `${safeFileName(payload.id)}-${safeFileName(att.name)}`;
      try {
        const data = await this.fetchWithTimeout(source, payload.platform);
        await saveInboxFile(cfg.workspaceDir, date, fileName, data);
        att.path = `inbox/${date}/${fileName}`;
        att.size = data.length;
        att.mimeType ??= mime.getType(att.name) ?? undefined;
        if (att.mimeType === undefined) delete att.mimeType;
      } catch (e) {
        // DESIGN 6.5 step 7: a failed download is recorded as {error}; the message is still sent.
        const limit =
          cfg.attachmentTimeoutMs % 1000 === 0 ? `${cfg.attachmentTimeoutMs / 1000} 秒` : `${cfg.attachmentTimeoutMs} 毫秒`;
        att.error = e instanceof DownloadTimeout ? `下载超时（${limit}）` : errorText(e);
        this.deps.logger.warn({ seq, attachment: att.name, err: e }, "attachment download failed");
      }
    }
    delete payload.pending;
    log.updatePayload(seq, payload);
    delivery.pump();
  }

  private fetchWithTimeout(source: Attachment, platform: string): Promise<Buffer> {
    const timeoutMs = this.deps.cfg.attachmentTimeoutMs;
    const fetching = this.fetchBytes(source, platform, timeoutMs);
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new DownloadTimeout()), timeoutMs);
    });
    // A download that lost the race keeps running; its late result is dropped here.
    fetching.catch((e: unknown) => this.deps.logger.debug({ err: e }, "late attachment download result"));
    return Promise.race([fetching, timeout]).finally(() => clearTimeout(timer));
  }

  private async fetchBytes(source: Attachment, platform: string, timeoutMs: number): Promise<Buffer> {
    const opts = { platform: platform as unknown as PlatformName };
    if (source.data !== undefined) {
      return (await toBuffer(source.data, opts))!;
    }
    if (source.fetchData) {
      return (await toBuffer(await source.fetchData(), opts))!;
    }
    if (source.url) {
      // QQ attachments carry only a URL (contracts 9.4).
      return downloadAttachment(source.url, { adapter: platform, timeoutMs });
    }
    throw new Error("平台没有提供附件内容");
  }
}
