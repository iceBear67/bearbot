---
name: bearbot-middleware
description: 用 bearbot SDK 编写处理 IM 消息的扩展时使用：订阅者、入站与出站中间件（防抖、合并、过滤、改写、补充上下文、不经过模型的命令）、回合事件、prompt() 定时任务、主动发送消息、扩展之间的约定。包括消息的类型定义、处理顺序与完整示例。
---

# bearbot 中间件与订阅者

扩展通过 bearbot SDK 取得 `bearbot-im` 的 API，在消息交给模型之前处理它们，或者在回复发出之前改写回复。扩展的写入位置、加载规则、检查与应用流程见技能 `bearbot-extension-dev`，本技能只讲消息处理。

## 1. 取得 API

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "my-extension" });
  bearbot.onMessage((msg) => {
    console.log(`${msg.sender.name}: ${msg.text}`);
  });
}
```

`useBearbot(pi, {name})` 立即返回一个句柄。它在工厂函数中注册 `pi.on("session_start", …)`，在其中执行 `pi.events.emit("bearbot:connect", {apiVersion: 1, name, resolve})`，`bearbot-im` 的处理函数同步调用 `resolve(api)`。在此之前对句柄的注册调用（`use`、`onMessage` 等）先缓存，取得 API 后按顺序生效。`session_start` 中没有得到响应时抛出错误“bearbot-im 未加载”。`bearbot-im` 通过 `-e` 最先加载，它的处理函数总是先于其他扩展注册。

## 2. 消息的处理过程

```
gateway 通知 message
  │
  ├─ 1. 订阅者：依次调用 onMessage 监听函数（只读，等待全部完成）
  ├─ 2. 中间件：按 order 依次调用，每个中间件通过 next(msg) 交给下一个
  ├─ 3. 路由：drop → 确认；trigger = false → 放入该会话串的旁观缓冲区，确认；trigger = true → 放入派发队列
  └─ 4. 派发：agent 空闲时，从队列取出最早一条消息所在会话串的全部待派发消息，组成一轮
```

- **一个会话、一个派发队列**：所有绑定频道（多个群和私聊）的触发消息，以及扩展通过 `prompt()` 发起的请求，进入同一个派发队列，在同一个会话中一轮一轮依次处理，同一时间最多一轮。每一轮只包含来自同一个会话串的消息，回复只发往这个会话串。
- **确认**：每条消息在被路由处理（放入旁观缓冲区、派发、被丢弃）后才算完成，`bearbot-im` 向 gateway 确认已经连续完成的最大 `seq`。被中间件扣留的消息没有完成，进程重启后 gateway 会补发它们。
- **旁观缓冲区**：每个会话串保留最近 `observe_limit`（默认 30）条 `trigger = false` 的消息，派发时随同一会话串的消息一起交给模型（`<observed>`），然后清空。
- **本轮角色**：本轮所有触发消息发送者中最低的角色（`member` 低于 `admin`）。`member` 回合中的工具调用由 classifier 检查。
- **回合进行中到达的触发消息**：同时满足三个条件时插入当前回合：消息所在的会话串就是本轮的 `origin`；该频道绑定的 `interrupt` 为 `steer`；发送者角色不低于本轮角色。其他情况留在队列中排队。
- **回复**：绑定的 `reply_mode = final` 时发送本轮最后一条助手消息的文本，`each` 时每条包含文本的助手消息都发送；文本为空或等于 `NO_REPLY` 时不发送。发送前经过出站中间件。
- **交给模型的文本**：中间件写入 `msg.context` 的内容渲染为 `<context from="<from>">…</context>`，放在本轮的 `<bearbot-turn>` 中。`from` 原样取自中间件写入的条目，`bearbot-im` 不会填写或检查它，模型却把它当作中间件所属扩展的名称，所以中间件必须把 `from` 填成自己扩展的名称（传给 `useBearbot` 的 `name`）；`stale` 的消息只出现在 `<observed>` 中；命令消息带 `command`、`args` 属性。

## 3. 类型（`/opt/bearbot/sdk/index.ts`）

```ts
export type Role = "admin" | "member" | "system";

export interface Sender {
  id: string;                 // 身份，例如 "telegram:123"
  name: string;
  role: "admin" | "member";
  isBot: boolean;
}

export interface Attachment {
  type: "image" | "file" | "audio" | "video";
  name: string;
  path?: string;              // 相对工作区目录，例如 "inbox/2026-10-26/x.pdf"
  size?: number;
  mimeType?: string;
  error?: string;             // 下载失败的原因
}

export interface InboundMessage {
  seq: number[];              // 合并后包含多条原始消息的 seq
  kind: "message" | "command" | "action" | "reaction";
  id: string;
  platform: string;
  channel: { id: string; name?: string; isDM: boolean };
  thread: string;             // 会话串 id，回复目标
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
  trigger: boolean;           // 是否触发 agent；gateway 给出初始值，中间件可以修改
  context: { from: string; text: string }[];   // 追加给模型的内容
  meta: Record<string, unknown>;               // 中间件之间传递数据
}

export type Next = (msg: InboundMessage) => void;

export interface MiddlewareControl {
  drop(msg: InboundMessage, reason: string): void;      // 丢弃，不再交给后续中间件
  merge(msgs: InboundMessage[]): InboundMessage;         // 合并为一条：seq 合并，文本按时间顺序拼接，附件合并
  reply(msg: InboundMessage, text: string): Promise<void>; // 直接回复，不经过模型
}

export type InboundMiddleware =
  (msg: InboundMessage, next: Next, ctl: MiddlewareControl) => void | Promise<void>;

export interface OutboundReply {
  target: string;
  markdown: string;
  files: string[];
  turn: TurnInfo | null;      // im_send 与 post() 发起的发送为 null
}

export type OutboundMiddleware =
  (reply: OutboundReply, next: (reply: OutboundReply) => void, ctl: { drop(reason: string): void }) => void | Promise<void>;

export interface TurnInfo {
  origin: string;
  role: Role;
  senders: Sender[];
  messages: InboundMessage[];
}

export interface PromptRequest {
  text: string;
  role: "admin" | "member";
  origin?: string;            // 回复目标；不填时最终回复不发送
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

  prompt(request: PromptRequest): Promise<void>;          // 进入派发队列，与聊天消息同样排队
  post(target: string, message: { markdown?: string; files?: string[]; card?: unknown }): Promise<{ ref: string }>;
  edit(ref: string, message: { markdown?: string; card?: unknown }): Promise<void>;
  delete(ref: string): Promise<void>;
  react(ref: string, emoji: string, remove?: boolean): Promise<void>;
  history(target: string, options?: { limit?: number; before?: string }): Promise<InboundMessage[]>;
  bindings(): Binding[];
  isAdmin(identity: string): boolean;
  diagnostic(entry: { level: "error" | "warning"; message: string; detail?: string }): void;
}

export function useBearbot(pi: ExtensionAPI, options: { name: string }): BearbotApi;
```

字段说明：

- `kind`：`message` 普通消息；`command` 以 `/` 开头的命令（`/bb` 除外，它由 gateway 处理），`command = {name, args}`，例如 `{name: "/weather", args: "上海"}`，初始 `trigger` 为 `true`；`action` 卡片按钮；`reaction` 表情。`action`、`reaction` 的初始 `trigger` 为 `false`。
- `trigger` 的初始值由绑定的触发方式决定：`mention` 时为“提及了 bot 或者是私聊”，`all` 时为 `true`，`prefix` 时为文本是否以前缀开头（前缀已去掉）。被编辑的消息为 `false`。
- `stale`：bot 长时间不可用后补发的过期消息，`trigger` 已改为 `false`，只作为旁观上下文。
- `attachments[].path` 相对工作区目录；下载失败时只有 `error`。
- `channel.id`、`thread`：例如 Telegram 群 `telegram:-100123`，论坛话题 `telegram:-100123:7`，QQ 群 `qq:group/<id>`，控制台 `console:main`。

## 4. 入站中间件的语义

- 按 `order` 从小到大调用，`order` 相同时按注册顺序。`bearbot-im` 的路由排在最后。
- 中间件调用 `next(msg)` 把消息交给下一个中间件，可以在任何时候调用（例如定时器到期时），也可以把修改后或合并后的消息交给 `next`。
- 中间件既没有调用 `next` 也没有调用 `ctl.drop` 时，消息处于“扣留”状态。被扣留的消息没有完成确认，进程重启后会被补发；扣留超过 `hold_warn_after`（默认 10 分钟）时产生诊断。
- 合并：`ctl.merge(msgs)` 产生的消息带有所有原始消息的 `seq`。合并后的消息到达路由时，这些 `seq` 一起完成。被合并的消息各自的 `next` 不需要再调用。
- 中间件抛出异常（同步，或者返回的 Promise 被拒绝）时，`bearbot-im` 记录诊断，并以进入该中间件之前的消息调用下一个中间件，一个扩展的错误不会让 bot 不再响应。
- 中间件可以修改 `msg.text`、`msg.trigger`，向 `msg.context` 追加内容，在 `msg.meta` 中给后面的中间件留下数据。追加到 `msg.context` 的条目中，`from` 必须是扩展自己的名称（传给 `useBearbot` 的 `name`）：模型把 `from` 当作中间件所属扩展的名称，`bearbot-im` 不会填写或检查这个字段。
- `ctl.reply(msg, text)` 直接回复到 `msg.thread`，不经过模型；回复之后通常再调用 `ctl.drop`，或者把消息交给 `next` 让模型继续处理。
- 返回 `Unsubscribe` 函数，调用后移除这个中间件。

## 5. 出站中间件

`useOutbound(middleware, {order})` 按同样的规则执行，作用于 agent 的最终回复、`im_send` 和 `post()`。`reply.turn` 是产生这条回复的回合，`im_send` 与 `post()` 发起的发送为 `null`。中间件可以修改 `reply.markdown`、`reply.files`、`reply.target` 后调用 `next(reply)`，或者调用 `ctl.drop(reason)` 不发送。

## 6. 订阅者与回合事件

- `onMessage(listener)`：在中间件之前调用，收到的是只读的原始消息，用于记录、统计、触发外部动作。全部订阅者完成后消息才进入中间件，所以监听函数中不要做耗时的工作。
- `onTurnStart(listener)`：一轮开始，`TurnInfo` 中有回复目标 `origin`、本轮角色、发送者与消息。
- `onTurnEnd(listener)`：一轮结束，`result` 中有最终文本、是否被中止、错误信息。
- `onBindingsChange(listener)`：管理员绑定或解除绑定频道之后调用。

## 7. 发起回合与主动发送

- `prompt({text, role, origin?, images?})`：进入派发队列，与聊天消息同样排队。`role` 决定本轮角色（`member` 回合的工具调用经过 classifier）；`origin` 是最终回复的目标，不填时最终回复不发送。
- `post(target, {markdown?, files?, card?})`：直接发送，不经过模型。`target` 必须是已绑定频道的会话串或频道 id，或者某个管理员的通知会话串；`files` 是工作区中的文件；`card` 是 chat-sdk 的 `CardElement` JSON，按钮被点击后以 `kind = "action"` 的消息送回。返回的 `ref` 用于 `edit`、`delete`、`react`。
- `history(target, {limit?, before?})`：gateway 记录的聊天历史，包括 bot 自己发出的消息。
- 这些方法都返回 Promise。没有 `await` 的调用要加 `.catch(...)`，并用 `diagnostic()` 报告失败：未处理的 Promise 拒绝会终止 agent 进程。
- 试运行中（`bearbot.trial` 为 `true`）`post`、`edit`、`delete`、`react`、`history`、`prompt`、`bindings`、`isAdmin` 抛出错误“试运行中不能调用”；注册类方法照常工作。

## 8. 示例

### 防抖：合并同一发送者 2 秒内的触发消息

`/bearbot/workspace/.pi/extensions/debounce.ts`：

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot, type InboundMessage, type Next } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "debounce" });
  const pending = new Map<string, { msgs: InboundMessage[]; next: Next; timer: NodeJS.Timeout }>();

  bearbot.use((msg, next, ctl) => {
    if (!msg.trigger) return next(msg);
    const key = `${msg.thread}|${msg.sender.id}`;
    const entry = pending.get(key) ?? { msgs: [], next, timer: undefined as unknown as NodeJS.Timeout };
    clearTimeout(entry.timer);
    entry.msgs.push(msg);
    entry.next = next;
    entry.timer = setTimeout(() => {
      pending.delete(key);
      try {
        entry.next(ctl.merge(entry.msgs));
      } catch (error) {
        bearbot.diagnostic({
          level: "error",
          message: "debounce 合并消息失败",
          detail: error instanceof Error ? error.stack : String(error),
        });
      }
    }, 2000);
    pending.set(key, entry);
  }, { order: 100 });
}
```

定时器回调中的代码不在 `bearbot-im` 的异常处理范围内，抛出的异常会终止 agent 进程，所以这里自己捕获。

### 不经过模型的命令

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "ping" });

  bearbot.use(async (msg, next, ctl) => {
    if (msg.kind !== "command" || msg.command?.name !== "/ping") return next(msg);
    await ctl.reply(msg, "pong");
    ctl.drop(msg, "已由 ping 扩展处理");
  }, { order: 10 });
}
```

### 过滤与按内容决定是否回复

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "group-filter" });

  bearbot.use((msg, next, ctl) => {
    if (msg.sender.isBot) return ctl.drop(msg, "忽略其他机器人的消息");
    if (!msg.channel.isDM && msg.trigger && msg.sender.role === "member" && !/[?？]\s*$/.test(msg.text)) {
      msg.trigger = false;
    }
    next(msg);
  }, { order: 20 });
}
```

改为 `trigger = false` 的消息进入旁观缓冲区，下一轮作为上下文交给模型。

### 为模型补充信息

```ts
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "glossary" });

  bearbot.use(async (msg, next) => {
    if (msg.trigger && msg.text.includes("术语")) {
      const glossary = await readFile(join(bearbot.workspace.dir, "docs", "glossary.md"), "utf8");
      msg.context.push({ from: "glossary", text: glossary });
    }
    next(msg);
  }, { order: 50 });
}
```

中间件抛出异常（例如文件不存在）时，`bearbot-im` 记录诊断，并把进入这个中间件之前的消息交给下一个中间件。

### 改写回复

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "signature" });

  bearbot.useOutbound((reply, next) => {
    if (reply.turn !== null && reply.target.startsWith("qq:")) {
      reply.markdown = `${reply.markdown}\n\n（由 bearbot 自动回复）`;
    }
    next(reply);
  }, { order: 100 });
}
```

### 定时任务

用 `croner` 安排定时任务，用 `prompt()` 发起回合。扩展放在子目录中，按技能 `bearbot-extension-dev` 第 5 节写 `package.json`，在该目录执行 `npm install croner`：

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Cron } from "croner";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "daily-digest" });
  let job: Cron | undefined;

  pi.on("session_start", () => {
    if (bearbot.trial || job) return;
    job = new Cron("0 9 * * 1-5", () => {
      bearbot
        .prompt({ text: "整理昨天群里的讨论要点并发到群里", role: "member", origin: "telegram:-100123" })
        .catch((error: unknown) => {
          bearbot.diagnostic({
            level: "error",
            message: "daily-digest 发起回合失败",
            detail: error instanceof Error ? error.stack : String(error),
          });
        });
    });
  });

  pi.on("session_shutdown", () => {
    job?.stop();
    job = undefined;
  });
}
```

试运行中不安排定时任务；`session_shutdown` 中停止它。

## 9. 扩展之间的约定

- `pi.events` 上以 `bearbot:` 开头的事件名保留给 `bearbot-im`。
- 扩展要向其他扩展提供接口时，使用 `<扩展名>:<事件>` 形式的事件名，请求与响应的方式与 `bearbot:connect` 相同：在载荷中放回调函数。
- 依赖其他扩展的扩展，在 `session_start` 中建立联系，这时所有扩展都已加载完毕。

## 10. 可以用中间件与订阅者实现的功能

| 功能 | 实现方式 |
|---|---|
| 消息防抖、合并 | 入站中间件（`ctl.merge`） |
| 过滤（屏蔽某些成员、关键词、机器人消息） | 入站中间件调用 `ctl.drop` |
| 按内容决定是否回复（例如群内只回答问题） | 入站中间件修改 `msg.trigger` |
| 为模型补充信息（查缓存、查数据库、附上相关文档） | 入站中间件写入 `msg.context` |
| 自定义斜杠命令 | 入站中间件处理 `kind = "command"` 的消息，直接回复，或改写后交给模型 |
| 回复改写（格式转换、敏感词处理、翻译） | 出站中间件 |
| 定时任务、周期报告 | 扩展内的定时器加 `prompt()` |
| 外部系统推送到群（接收 webhook、轮询外部 API） | 扩展内启动 HTTP 服务或定时请求，用 `post()` 发送；对外端口需要在 `docker run` 中映射；试运行中不启动 |
| 交互卡片与按钮 | `post()` 发送 `card`，订阅者或中间件处理 `kind = "action"` 的消息 |
| 统计与记录 | 订阅者（`onMessage`、`onTurnEnd`） |
