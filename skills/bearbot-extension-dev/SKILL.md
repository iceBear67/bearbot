---
name: bearbot-extension-dev
description: 在 bearbot 中编写或修改 pi 扩展、技能、提示模板、MCP 配置、模型与设置时使用。说明 pi 文档的位置、bearbot SDK 的用法、写入位置与位置检查、扩展目录与 npm 依赖、会让 agent 进程无法启动或退出的写法、试运行、bearbot_check 与 bearbot_apply 的流程，以及在 AGENTS.md 中登记扩展。
---

# bearbot 扩展开发

你运行在 bearbot 的 agent 进程中：`pi --mode rpc`，工作目录 `/bearbot/workspace`，以 `bearbot` 用户身份运行，最先加载的是对接扩展 `bearbot-im`。你编写的扩展、技能、MCP 配置让你获得新的能力；改动经过检查与应用、重启 agent 进程之后才生效。扩展出错会让 agent 进程无法启动或退出，所以按本技能的规则编写，并且每次都完成检查。

## 1. 文档与参考

pi 安装在 `/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/`：

| 位置 | 内容 |
|---|---|
| `README.md` | 总览 |
| `docs/extensions.md` | 扩展的生命周期、事件、工具、工具暴露方式、错误处理 |
| `docs/skills.md`、`docs/prompt-templates.md` | 技能与提示模板的格式 |
| `docs/mcp.md` | `mcp.json` 的格式、MCP 工具的暴露方式（默认只能从 codemode 脚本调用） |
| `docs/settings.md`、`docs/models.md`、`docs/custom-provider.md` | 设置项、自定义模型服务 |
| `docs/packages.md`、`docs/codemode.md`、`docs/sdk.md`、`docs/rpc.md` | 包、codemode 脚本、SDK、RPC 协议 |
| `examples/extensions/` | 扩展示例：`hello.ts`（最小的工具与命令）、`todo.ts`（工具与状态）、`permission-gate.ts`（`tool_call`）、`prompt-customizer.ts`（`before_agent_start` 修改系统提示）、`dynamic-tools.ts`、`truncated-tool.ts`（截断过长的工具输出）、`event-bus.ts`（`pi.events`）、`with-deps/`（带 npm 依赖的扩展目录） |
| `dist/core/extensions/types.d.ts`、`dist/index.d.ts` | `ExtensionAPI`、事件、`ToolDefinition`、`ExtensionContext` 等全部类型 |

编写之前完整阅读相关的文档与示例，并按其中的链接继续阅读。bearbot 的 API 在 `/opt/bearbot/sdk/index.ts` 中；中间件、订阅者、`prompt()` 的语义与示例见技能 `bearbot-middleware`。

## 2. 写入位置

| 目标 | 写入位置 | 生效方式 |
|---|---|---|
| 扩展、技能、提示模板、MCP、模型与设置 | `/bearbot/workspace/.pi/extensions/`、`.pi/skills/`、`.pi/prompts/`、`.pi/mcp.json`、`.pi/settings.json` | 检查与应用 |
| 系统提示 | `/bearbot/workspace/.pi/SYSTEM.md`（替换默认系统提示）、`.pi/APPEND_SYSTEM.md`（追加） | 检查与应用 |
| 自定义模型服务 | `/bearbot/profile/agent/models.json` | 检查与应用 |
| 扩展需要的 npm 依赖 | 扩展目录内的 `package.json`，在该目录执行 `npm install`（第 5 节） | 与扩展相同 |
| 持久化的命令行工具 | `/bearbot/profile/bin/`（位于 `PATH` 最前） | 不需要应用 |
| Python 包与脚本 | `uv pip install` 或 `pip install` 安装到 `/bearbot/profile/python`（已经是当前的虚拟环境）；脚本放在工作区或 `/bearbot/profile/bin/` | 不需要应用 |
| 记忆与资料 | `/bearbot/workspace/AGENTS.md`、`/bearbot/workspace/docs/` | 不需要应用 |
| classifier 补充规则（只在 `admin` 回合中修改） | `/bearbot/workspace/CLASSIFIER.md` | 不需要应用，下一个 `member` 回合生效 |

**位置检查**：`/bearbot/profile/agent/` 中存在非空的 `extensions/`、`skills/`、`prompts/`，或者存在 `mcp.json`、`AGENTS.md`、`AGENTS.override.md`、`AGENTS.MD`、`CLAUDE.md`、`CLAUDE.MD`、`SYSTEM.md`、`APPEND_SYSTEM.md` 时，检查失败，agent 进程也不会启动。pi 会加载这些位置的内容，它们却不在快照与检查的范围内。这些内容一律写到工作区的对应位置。

所有扩展都放在 `.pi/extensions/` 中。不要在 `.pi/settings.json` 的 `extensions`、`packages` 中引用其他位置的扩展或包，它们不在类型检查与快照的范围内。

## 3. 扩展的形式与加载规则

pi 按以下规则加载 `.pi/extensions/` 中的扩展：

- 第一层的 `.ts`、`.js` 文件，每个文件是一个扩展，例如 `.pi/extensions/weather.ts`。
- 第一层的子目录是一个扩展：子目录中有 `package.json`，且其中的 `pi.extensions` 列出了存在的文件时，加载这些文件；否则加载子目录中的 `index.ts`，再否则加载 `index.js`；都没有时整个子目录不加载。子目录中的其他文件只能被入口文件导入。
- 只看一层：子目录的子目录不会被自动加载。
- 跳过以 `.` 开头的条目、`node_modules`，以及被 `.pi/extensions/` 中的 `.gitignore`、`.ignore`、`.fdignore` 忽略的条目。暂时停用一个扩展时，把它移到 `.pi/extensions/.disabled/` 下。
- 不要在 `.pi/extensions/` 本身直接放 `index.ts`、`index.js` 或带 `pi.extensions` 的 `package.json`：那样只会加载它们，目录中的其他扩展全部不加载。

扩展模块的默认导出是工厂函数 `export default function (pi: ExtensionAPI) { … }`，可以是 `async` 函数。工厂函数中注册事件处理函数（`pi.on`）、工具（`pi.registerTool`）、命令（`pi.registerCommand`）。

类型检查的范围是 `.pi/extensions/` 下除了以 `.` 开头的条目和 `node_modules` 之外的全部 `.ts` 文件，包括 pi 不会加载的文件（被忽略的条目、没有入口的子目录），这些文件中的类型错误同样使检查失败。`.js` 文件不做类型检查，它们的语法错误由试运行发现。

## 4. 可以导入的模块

- pi 在运行时提供以下模块，不需要安装：`@earendil-works/pi-coding-agent`、`@earendil-works/pi-ai`、`@earendil-works/pi-ai/compat`、`@earendil-works/pi-ai/models`、`@earendil-works/pi-ai/oauth`、`@earendil-works/pi-ai/providers/all`、`@earendil-works/pi-tui`、`@earendil-works/pi-agent-core`、`typebox`、`typebox/value`、`typebox/compile`，以及旧名称 `@mariozechner/pi-coding-agent`、`@mariozechner/pi-ai`（及同样的子路径）、`@mariozechner/pi-tui`、`@mariozechner/pi-agent-core`、`@sinclair/typebox`、`@sinclair/typebox/value`、`@sinclair/typebox/compile`。这些名称的其他子路径（例如 `typebox/guard`）不提供。
- Node 内置模块：`node:fs`、`node:http`、`node:child_process` 等。
- bearbot SDK：`import { useBearbot } from "/opt/bearbot/sdk/index.ts";`（绝对路径，带 `.ts`）。
- 扩展内的其他文件：相对路径带 `.ts` 扩展名，例如 `import { format } from "./format.ts";`。
- 其他 npm 包：安装到扩展目录（第 5 节）。
- 每个扩展导入的模块都是自己的一份实例，模块级变量不会在扩展之间共享。扩展之间通信只能用 `pi.events`（第 7 节）。
- 工具参数中的字符串枚举用 `@earendil-works/pi-ai` 的 `StringEnum(["a", "b"] as const)`，部分模型服务不支持 `Type.Union` 生成的 `anyOf`。

## 5. 扩展目录与 npm 依赖

需要 npm 包的扩展使用子目录，在子目录中写 `package.json`，然后在这个目录中执行 `npm install`：

```json
{
  "name": "weather",
  "private": true,
  "type": "module",
  "dependencies": { "croner": "^10.0.0" },
  "peerDependencies": {
    "@earendil-works/pi-coding-agent": "*",
    "typebox": "*"
  },
  "peerDependenciesMeta": {
    "@earendil-works/pi-coding-agent": { "optional": true },
    "typebox": { "optional": true }
  }
}
```

- pi 包与 `typebox` 写在 `peerDependencies` 中，值为 `"*"`，并且在 `peerDependenciesMeta` 中标为 `optional: true`。不写 `peerDependenciesMeta` 时，npm 会把一份完整的 pi（约 168 MB）和另一个版本的 typebox 装进扩展目录。运行时与类型检查使用的都是全局安装的 pi。
- 没有 `pi` 字段时加载 `index.ts`；需要别的入口时写 `"pi": { "extensions": ["./main.ts"] }`。
- `package.json` 与 `package-lock.json` 进入快照，`node_modules/` 不进入。从快照恢复后 `package.json` 有变化时，daemon 在该目录执行 `npm ci`（有 lock 文件时）或 `npm install`，所以保留 `package-lock.json`。
- 编译原生模块所需的 `build-essential` 与 Python 已经安装。

## 6. bearbot SDK

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "greeter" });

  bearbot.onMessage((msg) => {
    console.log(`收到 ${msg.sender.id} 在 ${msg.thread} 的消息`);
  });

  pi.on("session_start", () => {
    if (bearbot.trial) return;
    console.log(`工作区：${bearbot.workspace.dir}`);
  });
}
```

- `useBearbot(pi, { name })` 立即返回一个句柄，`name` 是扩展的名称，出现在诊断（来源 `extension:<name>`）、`bearbot_status` 列出的中间件与订阅者，以及扩展发起的 `kind="prompt"` 回合的 `extension` 属性中。中间件写入 `msg.context` 的条目要自己把 `from` 填成这个名称（技能 `bearbot-middleware`）。它在工厂函数中注册了自己的 `session_start` 处理函数，在其中通过 `pi.events` 向 `bearbot-im` 取得 API；在此之前对句柄的注册调用（`use`、`onMessage` 等）先缓存，取得 API 后按顺序生效。`bearbot-im` 没有加载时，`session_start` 中抛出错误“bearbot-im 未加载”。
- 在工厂函数开头调用 `useBearbot`，再注册自己的 `session_start` 处理函数：同一个扩展的处理函数按注册顺序执行，这样在你的 `session_start` 中 API 已经可用。注册类方法以外的成员（`trial`、`workspace`、`prompt`、`post`、`bindings()`、`isAdmin()`、`diagnostic()` 等）在取得 API 之前使用会抛出错误“bearbot-im 尚未连接：除注册类方法外，bearbot API 要在 session_start 之后使用”，所以只在 `session_start` 及之后使用它们，不要在工厂函数中使用。
- API（完整类型见 `/opt/bearbot/sdk/index.ts`）：

| 成员 | 作用 |
|---|---|
| `apiVersion`、`workspace.dir`、`trial` | API 版本（1）、工作区目录、是否在试运行中 |
| `onMessage(listener)` | 订阅者：每条入站消息在中间件之前交给它，只读 |
| `onTurnStart(listener)`、`onTurnEnd(listener)` | 每一轮开始与结束；结束时带最终文本、是否中止、错误 |
| `onBindingsChange(listener)` | 绑定的频道变化 |
| `use(middleware, { order })`、`useOutbound(middleware, { order })` | 入站与出站中间件（技能 `bearbot-middleware`） |
| `prompt({ text, role, origin?, images? })` | 发起一轮：进入派发队列，与聊天消息同样排队；`role` 为 `admin` 或 `member`，`origin` 为回复目标，不填时最终回复不发送 |
| `post(target, { markdown?, files?, card? })` | 直接发送消息，返回 `{ ref }`；`target` 必须是已绑定频道的会话串或频道，或者某个管理员的通知会话串；`files` 是工作区中的文件路径 |
| `edit(ref, …)`、`delete(ref)`、`react(ref, emoji, remove?)` | 编辑、删除已发送的消息，添加或移除表情 |
| `history(target, { limit?, before? })` | gateway 记录的聊天历史 |
| `bindings()`、`isAdmin(identity)` | 当前绑定的频道、身份是否为管理员 |
| `diagnostic({ level, message, detail? })` | 报告问题：下一轮作为诊断交给你；`error` 等级同时通知管理员 |

- 不要直接调用 `pi.sendUserMessage`：`bearbot-im` 只放行自己发起的输入，其他输入被丢弃，并记录诊断“请通过 bearbot SDK 的 prompt() 发起回合”。发起回合一律用 `bearbot.prompt()`，这样每一轮都有明确的角色与回复目标。
- `prompt()`、`post()` 等返回 Promise。没有 `await` 的调用必须加 `.catch(...)`（第 8 节）。

## 7. 扩展之间的约定

- `pi.events` 上以 `bearbot:` 开头的事件名保留给 `bearbot-im`。
- 扩展要向其他扩展提供接口时，使用 `<扩展名>:<事件>` 形式的事件名，请求与响应的方式与 `bearbot:connect` 相同：在载荷中放回调函数，处理方同步调用它。
- 依赖其他扩展的扩展，在 `session_start` 中建立联系，这时所有扩展都已加载完毕。
- `pi.events.on` 的处理函数抛出的异常只写到 stderr，不会成为诊断，处理函数内部自己处理错误。

## 8. 会让 agent 进程无法启动或退出的写法

### 启动失败

以下情况 pi 以退出码 1 退出，agent 进程无法启动；检查的试运行能发现它们：

- 语法错误、模块顶层或工厂函数抛出异常、没有默认导出的工厂函数。
- 工具同名：两个扩展在工厂函数中注册同名工具时启动失败（stderr 中是 `Tool "<名称>" conflicts with <路径>`）。会这样冲突的有 `bearbot-im` 的 `im_send`、`im_history`、`im_targets`、`bearbot_status`、`bearbot_logs`、`bearbot_check`、`bearbot_apply`、`bearbot_snapshot`，以及其他扩展的工具（`bearbot_status` 列出当前全部工具）。
- 与内置工具同名不会启动失败，扩展的工具会替换掉内置工具：注册 `read`、`bash`、`edit`、`write`、`grep`、`find`、`ls`、`powershell` 时，扩展的工具直接取代同名的内置工具；注册 `codemode` 或 `tool_search` 时，pi 不加载对应的内置扩展，只在 stderr 写一条 `Warning: Extension package "builtin:…"`，检查照常通过。注册命令 `mcp` 时同样如此：pi 不加载内置的 MCP 扩展，`mcp.json` 中的服务器都不会连接。MCP 工具 `mcp__<服务器>__<工具>` 在连接服务器之后才注册，与它同名同样不会被检查发现。这些名称一律不要使用，工具名加上扩展名作为前缀，例如 `weather_today`。
- flag 同名（`pi.registerFlag`）同样导致启动失败。命令同名不会失败，但两个命令都会被改名为 `名称:1`、`名称:2`（命令 `mcp` 见上一条）。
- 工具的 `parameters` 不是对象 schema（用 `Type.Object({...})`）。

工厂函数与 `session_start` 处理函数中不要等待外部服务（网络请求、连接数据库）：pi 等这些函数全部完成后才算启动完成，超过 `start_timeout` 记为启动失败。工厂函数中也不要启动服务、socket、定时器、文件监视；它们在 `session_start` 中启动（不等待其完成），在 `session_shutdown` 中关闭，关闭操作要能重复执行。

### 运行期退出

pi 的 RPC 模式没有 `uncaughtException`、`unhandledRejection` 处理，以下情况会终止整个 agent 进程：

- 调用 `process.exit()`、`process.kill(process.pid)`、`ctx.shutdown()`。
- 定时器（`setTimeout`、`setInterval`）、socket 与 HTTP 服务的回调、`EventEmitter` 监听函数中抛出的异常。
- 未处理的 Promise 拒绝：没有 `await`、也没有 `.catch(...)` 的 Promise 失败。
- 同步代码长时间占住事件循环（死循环、长时间的同步计算或同步执行命令）：daemon 的心跳得不到响应，终止整个进程树。

这些回调中的代码自己捕获异常，并用 `bearbot.diagnostic()` 报告：

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { useBearbot } from "/opt/bearbot/sdk/index.ts";

export default function (pi: ExtensionAPI) {
  const bearbot = useBearbot(pi, { name: "uptime" });
  let timer: NodeJS.Timeout | undefined;

  async function probe(): Promise<number> {
    const response = await fetch("https://example.com", { signal: AbortSignal.timeout(10_000) });
    return response.status;
  }

  pi.registerTool({
    name: "uptime_probe",
    label: "uptime probe",
    description: "访问 https://example.com 并返回 HTTP 状态码。只读，不发送任何本地数据。",
    promptSnippet: "检查 example.com 是否可以访问",
    parameters: Type.Object({}),
    annotations: { readOnlyHint: true, openWorldHint: true },
    async execute() {
      const status = await probe();
      return { content: [{ type: "text", text: `HTTP ${status}` }], details: undefined };
    },
  });

  pi.on("session_start", () => {
    if (bearbot.trial || timer) return;
    timer = setInterval(() => {
      probe()
        .then(async (status) => {
          if (status !== 200) {
            await bearbot.post("telegram:-100123", { markdown: `example.com 返回 HTTP ${status}` });
          }
        })
        .catch((error: unknown) => {
          bearbot.diagnostic({
            level: "error",
            message: "uptime 定时检查失败",
            detail: error instanceof Error ? error.stack : String(error),
          });
        });
    }, 10 * 60 * 1000);
  });

  pi.on("session_shutdown", () => {
    clearInterval(timer);
    timer = undefined;
  });
}
```

不会终止进程的错误：`pi.on(...)` 注册的事件处理函数抛出的异常由 pi 捕获，作为 `extension_error` 诊断交给你；工具的 `execute` 抛出的异常成为交给模型的错误结果；`tool_call` 处理函数抛出异常时，这次工具调用被阻止。由它们启动的定时器与回调仍然要遵守上面的规则。

### 其他限制

- **试运行**：检查时以与正式运行相同的参数启动一个临时的 agent 进程（`BEARBOT_TRIAL=1`），它加载你的全部扩展，但不连接 gateway、不派发任何回合。扩展在启动时有对外动作的（发送消息、启动 HTTP 服务、安排定时任务、连接外部服务、写入文件），先检查 `bearbot.trial`，为 `true` 时不执行。试运行中注册类方法（`onMessage`、`use` 等）照常工作；`post`、`edit`、`delete`、`react`、`history`、`prompt`、`bindings`、`isAdmin` 抛出错误“试运行中不能调用”；`diagnostic()` 写到 stderr，进入检查报告。正式运行的进程同时存在，试运行中启动的服务会与它争用端口。
- **对话框**：agent 进程没有用户界面，`ctx.ui.select`、`ctx.ui.input`、`ctx.ui.editor` 立即得到 `undefined`，`ctx.ui.confirm` 得到 `false`；`ctx.ui.notify` 只写入 daemon 的日志。
- **会话与重新加载**：不要调用 `ctx.reload()`、`ctx.newSession()`、`ctx.switchSession()`、`ctx.fork()`。进程内重新加载不会读取修改过的扩展代码，加载错误也不会报告；改动一律通过检查与应用生效。
- **系统提示**：`before_agent_start` 中可以修改 `event.systemPromptOptions`。新增的 `sections` 名称必须匹配 `^[a-z][a-z0-9_-]*$` 且不能是 `preamble`，否则每一轮都会在开始前失败。不要修改 `sections.bearbot`，它属于 `bearbot-im`。
- **`pi.sendMessage`** 的参数必须带 `display`。
- **输出**：`console.log` 写入 agent 进程的 stderr 日志（`bearbot_logs {source: "stderr"}`）。工具返回的内容要截断过长的部分（`@earendil-works/pi-coding-agent` 的 `truncateHead`、`truncateTail`、`DEFAULT_MAX_BYTES`、`DEFAULT_MAX_LINES`，见 `examples/extensions/truncated-tool.ts`）。
- **成员回合中的工具**：`member` 回合中每一次工具调用都由 classifier 判断，扩展工具按工具描述、参数和 `annotations` 判断是否只读、是否有对外的副作用。工具描述要如实说明它做什么，`annotations` 如实填写 `readOnlyHint`、`destructiveHint`、`openWorldHint`。管理员可以用 `/bb classifier allow|deny <工具模式>` 为工具设置规则，或者让你在 `admin` 回合中把判断标准写进 `CLASSIFIER.md`。

## 9. 检查与应用

1. **接收指令**：管理员提出需求（本轮角色为 `admin`）。`write`、`edit`、`bearbot_*` 在 `member` 回合中被 classifier 拒绝，成员提出的改造需求请管理员发起。
2. **编写**：在第 2 节的位置编写或修改，并在 `AGENTS.md` 中登记（第 10 节）。
3. **检查**：调用 `bearbot_check`（参数 `{}`），它依次执行：
   - 位置检查（第 2 节）；
   - 类型检查：`strict`、`noEmit`、`skipLibCheck`、`allowImportingTsExtensions`、`module: "preserve"`、`moduleResolution: "bundler"`、`target`/`lib` 为 `es2024`、`types: ["node"]`，pi 提供的模块与 bearbot SDK 的类型都已映射。有错误时检查到此结束。任何文件有语法错误时只报告语法错误，修正后再检查才会出现类型错误；
   - 试运行启动：启动失败时报告中有退出码与 stderr；启动期间的 `extension_error` 也算失败；MCP 服务器连接失败只记入 `warnings`。
   报告 `{id, passed, location, typecheck, trial, reportPath}` 中 `passed` 为 `true` 才算通过。不通过时根据位置检查结果、类型错误、启动 stderr 或 `extension_error` 修改，然后再次检查。
4. **应用**：检查通过后调用 `bearbot_apply {summary}`，`summary` 写明改了什么、为什么改。daemon 重新执行一次完整的检查，通过后提交快照 `apply/<n>`，返回 `{accepted: true, snapshot}`，本轮结束后重启 agent 进程；检查不通过时返回 `{accepted: false, report}`，不做任何改动。只在 bot 正常运行时接受应用。在本轮的最终回复中告诉用户即将重启。
5. **重启**：本轮结束后 daemon 按原来的会话 id 重启 agent 进程，会话继续。
6. **结果确认**：新进程中 `bearbot-im` 发起一轮 `system` 角色的回合（`<bearbot-turn kind="applied" …>`），列出改动摘要与当前已加载的工具、命令、中间件。在这一轮中调用新工具、查看 `bearbot_status`，确认新功能按预期工作，再把结果告诉发起的用户。
7. **观察期**：应用后的观察期内进程意外退出会直接建立故障，由 recovery 修复；recovery 修复后你会收到一轮说明故障原因、修改与经验的回合，确认 recovery 的修改没有去掉用户需要的功能，必要时重新实现。运行期的 `extension_error`、中间件异常等作为诊断出现在后续的回合中，出现时优先处理。
8. **记录经验**：发现并修复错误后，把原因和教训写入 `AGENTS.md` 的“经验记录”。

管理员可以随时回滚。收到 `<bearbot-turn kind="rollback" …>` 的回合后，与被撤销的改动有关的任务全部暂停：不要重新实现或重新应用这些改动，等待管理员的指示。

## 10. 在 AGENTS.md 中登记

`/bearbot/workspace/AGENTS.md` 是长期记忆。每个自己编写的扩展在“扩展、技能与 MCP”一节中占一行：文件、用途、依赖、注意事项。技能与 MCP 服务器同样登记。改名、停用、删除时同步修改。例如：

```markdown
## 扩展、技能与 MCP
<!-- 每个自己编写的扩展一行：文件、用途、依赖、注意事项 -->
- .pi/extensions/debounce.ts：同一会话串中同一发送者 2 秒内的触发消息合并为一条；无依赖；入站中间件 order 100。
- .pi/extensions/weather/index.ts：工作日 9 点向 telegram:-100123 发送天气；依赖 croner；试运行中不安排定时任务；工具 weather_today。
- .pi/mcp.json 中的 docs：公司文档检索；通过 codemode 调用。
```

recovery 修复故障时会阅读这一节来了解每个扩展的用途，保留用户需要的功能。

## 11. 快照、日志与诊断

- `bearbot_snapshot {action: "list" | "diff" | "restore", ref?, ref2?, paths?}`：查看快照，比较两个快照（`git diff --stat`），把重启生效路径（`workspace/.pi/**`、`profile/agent/settings.json`、`profile/agent/models.json`）恢复到某个快照。恢复之后要再次应用才会生效。`paths` 是相对 `/bearbot` 的路径（例如 `workspace/.pi/extensions/weather.ts`；绝对路径和相对工作区的 `.pi/...` 写法都会被拒绝），只能是重启生效路径；不填 `paths` 时恢复全部重启生效路径。恢复后 `package.json` 有变化的扩展目录由 daemon 执行 `npm ci` 或 `npm install`。
- 快照历史：`git --git-dir=/bearbot/profile/bearbot/snapshots.git --work-tree=/bearbot log`，用 `diff`、`show` 对比与取回旧内容（例如 `git … show apply/3^:workspace/.pi/extensions/weather/index.ts`）。仓库只读。
- `bearbot_logs {source: "stderr" | "rpc" | "exits" | "diagnostics", n?}`：agent 进程的 stderr、RPC 事件记录、退出记录、诊断记录。
- `bearbot_status`：agent 状态、已加载的工具与命令、已注册的中间件与订阅者、待处理诊断、最近故障、尚未确认的回滚记录。
