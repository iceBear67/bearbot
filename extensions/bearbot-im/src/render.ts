import type { ImageContent, TextContent } from "@earendil-works/pi-ai";
import type { InboundMessage, Role } from "../../../sdk/index.ts";
import type { WireAttachment, WireMessage } from "./gateway-client.ts";

// Text given to the model (DESIGN 7.3). Every user-controlled string is escaped, so chat text cannot forge tags or
// attributes; role attributes are written only here.

export function escapeText(text: string): string {
  return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

export function escapeAttr(text: string): string {
  return escapeText(text).replace(/"/g, "&quot;");
}

function pad(value: number, width = 2): string {
  return String(value).padStart(width, "0");
}

/** ISO 8601 in local time with the numeric offset, for example 2026-10-26T14:02:11+08:00. */
export function localIso(date: Date): string {
  const offset = -date.getTimezoneOffset();
  const sign = offset >= 0 ? "+" : "-";
  const abs = Math.abs(offset);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

const ISO_PREFIX = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})/;

function splitTime(time: string): { date: string; clock: string } {
  const match = ISO_PREFIX.exec(time);
  if (!match) throw new Error(`消息时间不是 ISO 8601 格式：${time}`);
  return { date: match[1], clock: match[2] };
}

/** `HH:MM` when the message is from `refDate` (YYYY-MM-DD), otherwise `YYYY-MM-DD HH:MM`. */
export function timeLabel(time: string, refDate: string): string {
  const { date, clock } = splitTime(time);
  return date === refDate ? clock : `${date} ${clock}`;
}

export function messageFromWire(wire: WireMessage): InboundMessage {
  const message: InboundMessage = {
    seq: [...wire.seq],
    kind: wire.kind,
    id: wire.id,
    platform: wire.platform,
    channel: { ...wire.channel },
    thread: wire.thread,
    sender: { ...wire.sender },
    time: wire.time,
    text: wire.text,
    mentionsBot: wire.mentionsBot,
    edited: wire.edited,
    stale: wire.stale,
    attachments: wire.attachments.map((attachment) => ({ ...attachment })),
    trigger: wire.trigger,
    context: [],
    meta: {},
  };
  if (wire.replyTo) message.replyTo = { ...wire.replyTo };
  if (wire.command) message.command = { ...wire.command };
  if (wire.action) message.action = { ...wire.action };
  if (wire.reaction) message.reaction = { ...wire.reaction };
  return message;
}

/** Copy of a message that shares no mutable structure with the original (meta values are copied one level deep). */
export function cloneMessage(message: InboundMessage): InboundMessage {
  const copy: InboundMessage = {
    ...message,
    seq: [...message.seq],
    channel: { ...message.channel },
    sender: { ...message.sender },
    attachments: message.attachments.map((attachment) => ({ ...attachment })),
    context: message.context.map((entry) => ({ ...entry })),
    meta: { ...message.meta },
  };
  if (message.replyTo) copy.replyTo = { ...message.replyTo };
  if (message.command) copy.command = { ...message.command };
  if (message.action) copy.action = { ...message.action };
  if (message.reaction) copy.reaction = { ...message.reaction };
  return copy;
}

function attr(name: string, value: string | number | boolean): string {
  return ` ${name}="${escapeAttr(String(value))}"`;
}

function who(name: string, id: string): string {
  return `${name} (${id})`;
}

export function renderChannel(message: Pick<InboundMessage, "channel" | "platform">): string {
  let out = `<channel${attr("id", message.channel.id)}${attr("platform", message.platform)}`;
  if (message.channel.name !== undefined && message.channel.name !== "") out += attr("name", message.channel.name);
  out += `${attr("type", message.channel.isDM ? "dm" : "group")}/>`;
  return out;
}

function renderAttachment(attachment: WireAttachment | InboundMessage["attachments"][number]): string {
  let out = `<attachment${attr("type", attachment.type)}`;
  if (attachment.path !== undefined) out += attr("path", attachment.path);
  out += attr("name", attachment.name);
  if (attachment.size !== undefined) out += attr("size", attachment.size);
  if (attachment.mimeType !== undefined) out += attr("mime-type", attachment.mimeType);
  if (attachment.error !== undefined) out += attr("error", attachment.error);
  return `${out}/>`;
}

/** One `<message>` element. */
export function renderMessage(message: InboundMessage, extraAttrs: Record<string, string> = {}): string {
  let open = `<message${attr("id", message.id)}${attr("from", who(message.sender.name, message.sender.id))}`;
  open += `${attr("sender-role", message.sender.role)}${attr("time", message.time)}${attr("mention", message.mentionsBot)}`;
  if (message.sender.isBot) open += attr("bot", true);
  if (message.edited) open += attr("edited", true);
  if (message.kind === "command" && message.command) {
    open += `${attr("command", message.command.name)}${attr("args", message.command.args)}`;
  }
  if (message.kind === "action" && message.action) {
    open += `${attr("kind", "action")}${attr("action-id", message.action.actionId)}`;
    if (message.action.value !== undefined) open += attr("value", message.action.value);
  }
  if (message.kind === "reaction" && message.reaction) {
    open += `${attr("kind", "reaction")}${attr("emoji", message.reaction.emoji)}`;
    open += `${attr("added", message.reaction.added)}${attr("message-id", message.reaction.messageId)}`;
  }
  for (const [name, value] of Object.entries(extraAttrs)) open += attr(name, value);
  const lines = [`${open}>`];
  if (message.replyTo) {
    lines.push(`<reply-to${attr("from", who(message.replyTo.name, message.replyTo.sender))}>${escapeText(message.replyTo.text)}</reply-to>`);
  }
  if (message.text !== "") lines.push(escapeText(message.text));
  for (const attachment of message.attachments) lines.push(renderAttachment(attachment));
  lines.push("</message>");
  return lines.join("\n");
}

function observedBody(message: InboundMessage): string {
  if (message.kind === "action" && message.action) {
    const value = message.action.value === undefined ? "" : `，值 ${message.action.value}`;
    return `[点击了按钮 ${message.action.actionId}${value}]${message.text === "" ? "" : ` ${message.text}`}`;
  }
  if (message.kind === "reaction" && message.reaction) {
    const verb = message.reaction.added ? "添加了表情" : "移除了表情";
    return `[${verb} ${message.reaction.emoji}，消息 ${message.reaction.messageId}]`;
  }
  return message.text;
}

/** One line (plus attachment lines) inside `<observed>`. */
export function renderObservedLine(message: InboundMessage, refDate: string): string {
  const marks: string[] = [];
  if (message.sender.isBot) marks.push("（机器人）");
  if (message.edited) marks.push("（已编辑）");
  if (message.stale) marks.push("（过期）");
  const head = `[${timeLabel(message.time, refDate)}] ${escapeText(who(message.sender.name, message.sender.id))}${marks.join("")}`;
  const lines = [`${head}: ${escapeText(observedBody(message))}`];
  for (const attachment of message.attachments) lines.push(renderAttachment(attachment));
  return lines.join("\n");
}

function renderObserved(observed: InboundMessage[], refDate: string): string[] {
  if (observed.length === 0) return [];
  return ["<observed>", ...observed.map((message) => renderObservedLine(message, refDate)), "</observed>"];
}

function renderContexts(messages: InboundMessage[]): string[] {
  const lines: string[] = [];
  for (const message of messages) {
    for (const entry of message.context) {
      lines.push(`<context${attr("from", entry.from)}>${escapeText(entry.text)}</context>`);
    }
  }
  return lines;
}

function turnOpen(attrs: [string, string | null][]): string {
  let out = "<bearbot-turn";
  for (const [name, value] of attrs) if (value !== null) out += attr(name, value);
  return `${out}>`;
}

function refDateOf(messages: InboundMessage[], now: Date): string {
  const last = messages.at(-1);
  return last ? splitTime(last.time).date : splitTime(localIso(now)).date;
}

export interface MessageTurnInput {
  role: Role;
  origin: string;
  messages: InboundMessage[];     // trigger messages, in order
  observed: InboundMessage[];
  kind?: "steer";
  now?: Date;
}

/** `<bearbot-turn>` for chat messages; stale messages are always rendered as observed. */
export function renderMessageTurn(input: MessageTurnInput): string {
  const fresh = input.messages.filter((message) => !message.stale);
  const observed = [...input.observed, ...input.messages.filter((message) => message.stale)];
  const refDate = refDateOf(fresh.length > 0 ? fresh : observed, input.now ?? new Date());
  const first = input.messages[0] ?? input.observed[0];
  if (!first) throw new Error("回合中没有任何消息");
  const lines = [
    turnOpen([["role", input.role], ["origin", input.origin], ["kind", input.kind ?? null]]),
    renderChannel(first),
    ...renderObserved(observed, refDate),
    ...fresh.map((message) => renderMessage(message)),
    ...renderContexts(input.messages),
    "</bearbot-turn>",
  ];
  return lines.join("\n");
}

export interface PromptTurnInput {
  role: "admin" | "member";
  origin: string | null;
  extension: string;
  text: string;
  channel: Pick<InboundMessage, "channel" | "platform"> | null;
}

/** `<bearbot-turn kind="prompt">` for a request made through the SDK's prompt(). */
export function renderPromptTurn(input: PromptTurnInput): string {
  const lines = [
    turnOpen([["role", input.role], ["origin", input.origin], ["kind", "prompt"], ["extension", input.extension]]),
  ];
  if (input.channel) lines.push(renderChannel(input.channel));
  lines.push("<prompt>", escapeText(input.text), "</prompt>", "</bearbot-turn>");
  return lines.join("\n");
}

export interface SystemTurnInput {
  kind: "applied" | "incident" | "rollback";
  origin: string | null;
  attrs?: [string, string | null][];
  body: string[];                 // lines; escaped here
}

/** `<bearbot-turn role="system">` started by bearbot-im itself (DESIGN 9.2, 9.4, 9.6). */
export function renderSystemTurn(input: SystemTurnInput): string {
  return [
    turnOpen([["kind", input.kind], ["role", "system"], ["origin", input.origin], ...(input.attrs ?? [])]),
    ...input.body.map((line) => escapeText(line)),
    "</bearbot-turn>",
  ].join("\n");
}

/** Images of the given messages as ImageContent, in message order. */
export function collectImages(messages: InboundMessage[]): ImageContent[] {
  const images: ImageContent[] = [];
  for (const message of messages) {
    for (const attachment of message.attachments as WireAttachment[]) {
      if (attachment.type !== "image" || attachment.data === undefined || attachment.mimeType === undefined) continue;
      images.push({ type: "image", data: attachment.data, mimeType: attachment.mimeType });
    }
  }
  return images;
}

export function userContent(text: string, images: ImageContent[]): (TextContent | ImageContent)[] {
  return [{ type: "text", text }, ...images];
}

/** Text of an assistant message (text blocks joined). */
export function assistantText(content: readonly { type: string; text?: string }[]): string {
  return content
    .filter((block): block is { type: "text"; text: string } => block.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("");
}
