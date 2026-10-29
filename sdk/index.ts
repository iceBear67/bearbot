import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export type Role = "admin" | "member" | "system";

export interface Sender {
  id: string;                 // identity, for example "telegram:123"
  name: string;
  role: "admin" | "member";
  isBot: boolean;
}

export interface Attachment {
  type: "image" | "file" | "audio" | "video";
  name: string;
  path?: string;              // relative to the workspace dir, for example "inbox/2026-10-26/x.pdf"
  size?: number;
  mimeType?: string;
  error?: string;             // download failure
}

export interface InboundMessage {
  seq: number[];              // seq numbers of every original message (several after a merge)
  kind: "message" | "command" | "action" | "reaction";
  id: string;
  platform: string;
  channel: { id: string; name?: string; isDM: boolean };
  thread: string;             // thread id, reply target
  sender: Sender;
  time: string;               // ISO 8601
  text: string;
  mentionsBot: boolean;
  edited: boolean;
  stale: boolean;
  replyTo?: { id: string; sender: string; name: string; text: string };
  command?: { name: string; args: string };
  action?: { actionId: string; value?: string };
  reaction?: { emoji: string; added: boolean; messageId: string };
  attachments: Attachment[];
  trigger: boolean;           // whether the message triggers the agent; the gateway sets it, middleware may change it
  steer?: true;               // an admin's `!steer`: inserted into the sender's own turn whatever the binding's interrupt is
  context: { from: string; text: string }[];   // extra content for the model
  meta: Record<string, unknown>;               // data passed between middleware
}

export type Next = (msg: InboundMessage) => void;

export interface MiddlewareControl {
  drop(msg: InboundMessage, reason: string): void;      // drop, later middleware do not see it
  merge(msgs: InboundMessage[]): InboundMessage;         // one message from messages of one sender with one role:seq union, texts joined in time order, attachments joined
  reply(msg: InboundMessage, text: string): Promise<void>; // reply directly without the model
}

export type InboundMiddleware =
  (msg: InboundMessage, next: Next, ctl: MiddlewareControl) => void | Promise<void>;

export interface OutboundReply {
  target: string;
  markdown: string;
  files: string[];
  replyTo?: string;           // id of the message to quote (Telegram, QQ); set by im_send or post()
  turn: TurnInfo | null;      // null for im_send and post()
}

export type OutboundMiddleware =
  (reply: OutboundReply, next: (reply: OutboundReply) => void, ctl: { drop(reason: string): void }) => void | Promise<void>;

export interface TurnInfo {
  origin: string;
  role: Role;
  senders: Sender[];          // exactly one for a turn of chat messages, none for prompt() and system turns
  messages: InboundMessage[];
}

export interface PromptRequest {
  text: string;
  role: "admin" | "member";
  origin?: string;            // reply target; without it the final reply is not sent
  images?: { data: string; mimeType: string }[];
}

export interface Binding {
  channel: string;
  name?: string;
  platform: string;
  trigger: "mention" | "all" | "prefix";
  replyMode: "final" | "each";
}

export type Unsubscribe = () => void;

export interface BearbotApi {
  readonly apiVersion: 1;
  readonly workspace: { dir: string };
  readonly trial: boolean;

  onMessage(listener: (msg: Readonly<InboundMessage>) => void | Promise<void>): Unsubscribe;
  onTurnStart(listener: (turn: TurnInfo) => void): Unsubscribe;
  onTurnEnd(listener: (turn: TurnInfo, result: { text: string; aborted: boolean; error?: string }) => void): Unsubscribe;
  onBindingsChange(listener: (bindings: Binding[]) => void): Unsubscribe;

  use(middleware: InboundMiddleware, options: { order: number }): Unsubscribe;
  useOutbound(middleware: OutboundMiddleware, options: { order: number }): Unsubscribe;

  prompt(request: PromptRequest): Promise<void>;          // enters the dispatch queue like chat messages
  post(target: string, message: { markdown?: string; files?: string[]; card?: unknown; replyTo?: string }): Promise<{ ref: string }>;
  edit(ref: string, message: { markdown?: string; card?: unknown }): Promise<void>;
  delete(ref: string): Promise<void>;
  react(ref: string, emoji: string, remove?: boolean): Promise<void>;
  history(target: string, options?: { limit?: number; before?: string }): Promise<InboundMessage[]>;
  bindings(): Binding[];
  isAdmin(identity: string): boolean;
  diagnostic(entry: { level: "error" | "warning"; message: string; detail?: string }): void;
}

/** Payload of the `bearbot:connect` event on `pi.events`; bearbot-im answers by calling `resolve` synchronously. */
export interface BearbotConnectRequest {
  apiVersion: 1;
  name: string;
  resolve(api: BearbotApi): void;
}

export const BEARBOT_CONNECT_EVENT = "bearbot:connect";

const NOT_LOADED = "bearbot-im 未加载";
const NOT_CONNECTED = "bearbot-im 尚未连接：除注册类方法外，bearbot API 要在 session_start 之后使用";

/**
 * Returns a handle at once. Registrations made before the API is available are buffered and applied in order
 * when `session_start` connects to bearbot-im. Every extension has its own instance of this module, so all state
 * lives in the closure.
 */
export function useBearbot(pi: ExtensionAPI, options: { name: string }): BearbotApi {
  if (typeof options?.name !== "string" || options.name.trim() === "") {
    throw new Error("useBearbot 需要非空的 name");
  }
  const name = options.name;
  let api: BearbotApi | undefined;

  type Pending = { register: (target: BearbotApi) => Unsubscribe; unsubscribe: Unsubscribe | undefined; cancelled: boolean };
  const pending: Pending[] = [];

  const register = (registerOn: (target: BearbotApi) => Unsubscribe): Unsubscribe => {
    if (api) return registerOn(api);
    const entry: Pending = { register: registerOn, unsubscribe: undefined, cancelled: false };
    pending.push(entry);
    return () => {
      if (entry.unsubscribe) {
        entry.unsubscribe();
        return;
      }
      entry.cancelled = true;
      const index = pending.indexOf(entry);
      if (index >= 0) pending.splice(index, 1);
    };
  };

  const connected = (): BearbotApi => {
    if (!api) throw new Error(NOT_CONNECTED);
    return api;
  };

  pi.on("session_start", () => {
    // pi may emit session_start twice after a runtime replacement; the first answer stays in use.
    if (api) return;
    let answered: BearbotApi | undefined;
    const request: BearbotConnectRequest = {
      apiVersion: 1,
      name,
      resolve: (value: BearbotApi) => {
        if (answered === undefined) answered = value;
      },
    };
    pi.events.emit(BEARBOT_CONNECT_EVENT, request);
    if (!answered) throw new Error(NOT_LOADED);
    api = answered;
    for (const entry of pending.splice(0)) {
      if (!entry.cancelled) entry.unsubscribe = entry.register(answered);
    }
  });

  return {
    apiVersion: 1,
    get workspace() {
      return connected().workspace;
    },
    get trial() {
      return connected().trial;
    },
    onMessage: (listener) => register((target) => target.onMessage(listener)),
    onTurnStart: (listener) => register((target) => target.onTurnStart(listener)),
    onTurnEnd: (listener) => register((target) => target.onTurnEnd(listener)),
    onBindingsChange: (listener) => register((target) => target.onBindingsChange(listener)),
    use: (middleware, opts) => register((target) => target.use(middleware, opts)),
    useOutbound: (middleware, opts) => register((target) => target.useOutbound(middleware, opts)),
    prompt: async (request) => connected().prompt(request),
    post: async (target, message) => connected().post(target, message),
    edit: async (ref, message) => connected().edit(ref, message),
    delete: async (ref) => connected().delete(ref),
    react: async (ref, emoji, remove) => connected().react(ref, emoji, remove),
    history: async (target, opts) => connected().history(target, opts),
    bindings: () => connected().bindings(),
    isAdmin: (identity) => connected().isAdmin(identity),
    diagnostic: (entry) => connected().diagnostic(entry),
  };
}
