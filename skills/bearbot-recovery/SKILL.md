---
name: bearbot-recovery
description: bearbot recovery 的修复步骤。agent 进程无法启动、反复退出或在观察期内退出，daemon 启动 recovery 处理故障时使用：如何读取日志与故障记录、判断启动失败的原因、修复工作区与 profile、使用快照仓库、用 recovery_check 确认、用 recovery_report 提交报告。
---

# bearbot recovery 修复步骤

## 1. 你所处的情况

bearbot 的 agent 进程（`pi --mode rpc` 加载 `bearbot-im` 与工作区中 agent 自己编写的扩展）出了故障。daemon 已经：

1. 建立故障 `inc-<UTC 时间 yyyymmddhhmmss>`，把记录写入 `/bearbot/profile/bearbot/incidents/<故障id>/incident.json`；
2. 提交快照 `incident/<故障id>/before`，保存故障发生时的文件状态；
3. 通知 gateway 进入修复状态，gateway 对触发消息回复维护提示，并通知管理员；
4. 用独立的 recovery profile 启动你，第一条提示就是故障报告。

故障报告的内容：

```
故障 <id>
触发：<启动失败 | 观察期内退出 | 反复退出 | 管理员要求>
详情：…
退出码 / 信号：…
stderr 最后 200 行：…
rpc 事件最后 50 条：…
最近一次应用：快照 <ref>，摘要 <summary>，发起者 <identity>
相对 last-good 的改动：<git diff --stat>
此前的修复尝试：<每次的报告与验证失败原因>
进入维护状态的原因：<原因>
管理员说明：<repair 命令附带的文字>
快照仓库：git --git-dir=/bearbot/profile/bearbot/snapshots.git --work-tree=/bearbot …
```

触发的含义：

| 触发 | 含义 |
|---|---|
| 启动失败 | 启动前的位置检查不通过；进程在 `get_state` 响应之前退出；`start_timeout` 内没有 `get_state` 响应；`bearbot-im` 在 `ready_timeout` 内没有调用 `bridge/ready` |
| 观察期内退出 | 应用、修复、管理员回滚、恢复运行之后的观察期（`probation`）内，进程意外退出 |
| 反复退出 | `crash_window` 内意外退出的次数达到 `crash_limit` |
| 管理员要求 | 维护状态下没有未关闭的故障（例如回滚出错后进入的维护状态），管理员执行了 `repair [说明]`；“详情”中写明进入维护状态的原因，说明写在“管理员说明”中。有未关闭的故障时管理员执行 `repair`，触发保持这个故障原来的值，进入维护状态的原因写在“进入维护状态的原因”一行，说明同样写在“管理员说明”中 |

“进入维护状态的原因”一行只在管理员执行 `repair`、继续处理维护状态下未关闭的故障时出现，例如修复失败之后，或者故障之后的回滚出错之后。

报告之后的流程：

- 报告 `fixed`：daemon 提交快照 `incident/<故障id>/attempt-<n>`，执行一次检查。检查通过后关闭你，以这个快照启动 agent 进程，进入观察期。观察期结束后故障关闭，daemon 写入 `report.md`，把你的 `summary`、`changed_files`、`lesson` 交给 agent；agent 会确认你的修改没有去掉用户需要的功能，并把经验写入它的记忆。
- 检查不通过，而尝试次数还没有用尽（`[recovery] attempts`，默认 2）：daemon 把检查结果作为新的提示发给仍在运行的你，对话继续（你的进程已经退出时，daemon 以同一个会话 id 启动新的 recovery 进程）。
- 检查通过但 agent 进程启动失败，而尝试次数还没有用尽：daemon 在启动 agent 之前已经关闭了你，这时它以同一个会话 id 启动新的 recovery 进程，对话继续，启动失败的信息作为新的提示发给你。两种情况下都在原来的基础上继续修复，然后再次检查和报告。
- 次数用尽，或者报告 `cannot_fix`：bot 进入维护状态，管理员可以执行 `/bb repair [说明]`（尝试次数重新计算）、`/bb rollback [快照]`、`/bb resume`。
- 一轮结束时没有报告：daemon 再发送一次提示要求报告；仍然没有报告时按 `cannot_fix` 处理。daemon 每发出一条提示（故障报告、要求报告的提醒、检查或启动失败之后的新提示），等待报告的时间上限都是 `[recovery] timeout`（默认 30 分钟）；超过上限时 daemon 中止这一轮，按 `cannot_fix` 处理。

## 2. 目录与权限

当前目录是 `/bearbot`，你以 `bearbot` 用户身份运行。

| 路径 | 内容 | 处理方式 |
|---|---|---|
| `workspace/.pi/extensions/` | agent 编写的扩展 | 修复；无法修复时移到 `workspace/.pi/extensions/.disabled/` |
| `workspace/.pi/skills/`、`workspace/.pi/prompts/` | agent 的技能与提示模板 | 需要时修复 |
| `workspace/.pi/settings.json`、`workspace/.pi/mcp.json`、`workspace/.pi/SYSTEM.md`、`workspace/.pi/APPEND_SYSTEM.md` | 项目设置、MCP 服务器、系统提示 | 需要时修复 |
| `workspace/AGENTS.md` | agent 的记忆，“扩展、技能与 MCP”一节登记了每个扩展的文件、用途、依赖与注意事项 | 阅读它了解扩展的用途；位置检查要求合并 profile 中的 `AGENTS.md`、`AGENTS.override.md`、`AGENTS.MD`、`CLAUDE.md`、`CLAUDE.MD` 时才修改 |
| `workspace/CLASSIFIER.md` | classifier 补充规则 | 不编辑 |
| `profile/agent/settings.json`、`profile/agent/models.json` | agent 的 pi 设置与自定义模型服务 | 需要时修复 |
| `profile/agent/auth.json` | 模型服务凭据 | 不修改 |
| `profile/agent/sessions/` | agent 的会话文件 | 不修改 |
| `profile/bearbot/incidents/<故障id>/` | `incident.json`（触发事件、退出码与信号、stderr 最后 200 行、rpc 事件最后 50 条、最近一次应用、相对 `last-good` 的 `git diff --stat`），`attempts/<n>.md`（每次修复尝试） | 阅读 |
| `profile/bearbot/checks/<检查id>/` | `report.json`、`tsc.out`、`trial-stderr.log` | 阅读 |
| `profile/bearbot/snapshots.git` | 快照仓库，属于 root | 只能用 git 查看；恢复用 `recovery_snapshot` |
| `/opt/bearbot/sdk/index.ts` | bearbot SDK，扩展用 `useBearbot` 取得 `bearbot-im` 的 API | 阅读，不能修改 |
| `/opt/bearbot/extensions/bearbot-im/` | 对接扩展 | 阅读，不能修改 |
| `/opt/bearbot/skills/` | 自带技能，`bearbot-extension-dev` 说明了扩展的写法与限制 | 阅读 |
| `/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/` | pi：`docs/`、`examples/extensions/`、`dist/core/extensions/types.d.ts` | 阅读 |

重启生效路径是 `workspace/.pi/**`、`profile/agent/settings.json`、`profile/agent/models.json`。快照恢复只恢复这些路径；`AGENTS.md`、`CLASSIFIER.md` 不会被恢复。

## 3. 工具

### `recovery_logs`

参数 `{source, kind, n?}`，返回最后 `n` 行（默认 200）：

| source | kind | 内容 |
|---|---|---|
| `agent` | `stderr` | agent 进程的标准错误输出。扩展的 `console.log`、加载错误、未捕获的异常都在这里 |
| `agent` | `rpc` | RPC 事件记录，每行一个 JSON：`extension_error`（扩展处理函数抛出的异常，带 `extensionPath`、`event`、`error`）、`agent_start`、`agent_settled`、带 `stopReason: "error"` 的助手消息结束事件，都带 `at` 时间 |
| `agent` | `exits` | 每次进程退出一行：`{"at","pid","code","signal","expected","stderrTail"}`。`expected` 为 `false` 的是意外退出 |
| `gateway` | `stderr` | gateway 日志（pino JSON） |
| `daemon` | `stderr` | daemon 日志：状态变化、启动失败的原因（例如 `get_state` 超时、`ready_timeout`、位置检查结果）、心跳超时 |

### `recovery_check`

参数 `{}`。执行与 agent 的 `bearbot_check` 相同的检查，返回报告：

```
{
  "id": "chk-…",
  "passed": true | false,
  "location":  { "passed", "violations": [路径…], "message" },
  "typecheck": { "ran", "passed", "errors": [{ "file", "line", "column", "code", "message" }], "output" },
  "trial":     { "ran", "passed", "exitCode", "signal", "timedOut", "stderr", "extensionErrors": [{ "extensionPath", "event", "error" }], "warnings": [] },
  "reportPath": "…/checks/<id>/report.json"
}
```

- 位置检查：`profile/agent/` 中存在非空的 `extensions/`、`skills/`、`prompts/`，或者存在 `mcp.json`、`AGENTS.md`、`AGENTS.override.md`、`AGENTS.MD`、`CLAUDE.md`、`CLAUDE.MD`、`SYSTEM.md`、`APPEND_SYSTEM.md` 时不通过，`violations` 列出这些路径。
- 类型检查：用 TypeScript 检查 `workspace/.pi/extensions/` 下除了以 `.` 开头的条目和 `node_modules` 之外的全部 `.ts` 文件（`strict`）。任何文件有语法错误时只报告语法错误，修正后再检查才会出现类型错误。`.js` 文件不在类型检查范围内，它们的语法错误由试运行发现。
- 试运行：以 agent 进程的参数和环境（另加 `BEARBOT_TRIAL=1`）启动一次 pi，等待 `get_state` 响应。类型检查有错误时检查到此结束，不进行试运行。`exitCode`、`stderr` 说明加载失败的原因；启动期间的 `extension_error` 也算失败；MCP 服务器连接失败只记入 `warnings`，不影响结果。
- 某一步的 `ran` 为 `false` 表示这一步没有执行。
- 通过条件：位置检查通过、类型检查没有错误、试运行启动成功、没有 `extension_error`。

### `recovery_snapshot`

参数 `{action, ref?, ref2?, paths?}`：

- `list`：最近 20 个快照引用，带提交时间、摘要、发起者；同时列出 `last-good`、`runningSnapshot` 指向的提交，以及不指定快照回滚时的默认目标（例如 `apply/12^`）。
- `diff`：两个快照之间的 `git diff --stat`；只给 `ref` 时与它的父提交比较。
- `restore`：把重启生效路径恢复到 `ref`。`paths` 是相对 `/bearbot` 的路径（例如 `workspace/.pi/extensions/weather.ts`；绝对路径和相对工作区的 `.pi/...` 写法都会被拒绝），只能是重启生效路径；不填 `paths` 时恢复全部重启生效路径。恢复后 `package.json` 有变化的扩展目录由 daemon 执行 `npm ci` 或 `npm install`。

快照引用只接受这些形式，末尾可以加 `^` 表示父提交：

| 引用 | 含义 |
|---|---|
| `init` | 快照仓库创建时的状态 |
| `apply/<n>` | 第 n 次应用时的状态，包含这次应用的改动；`apply/<n>^` 是这次应用之前的状态 |
| `incident/<故障id>/before`、`incident/<故障id>/attempt-<n>` | 故障发生时、每次修复后的状态 |
| `rollback/<n>/before`、`rollback/<n>/after` | 管理员回滚之前、之后的状态 |
| `resume/<n>` | 管理员在维护状态下恢复运行时的状态 |
| `last-good` | 最近一次度过观察期的状态 |

也可以使用 40 位十六进制的提交哈希。

### `recovery_report`

见第 7 节。

## 4. 修复步骤

1. 阅读故障报告：触发、退出码与信号、stderr、rpc 事件、最近一次应用、相对 `last-good` 的改动、此前的修复尝试、管理员说明。
2. 补充信息：
   - `recovery_logs {source: "agent", kind: "stderr", n: 500}`，必要时再看 `kind: "exits"`、`kind: "rpc"`；
   - `recovery_logs {source: "daemon", kind: "stderr"}` 查看 daemon 记录的启动失败原因；
   - 阅读 `incidents/<故障id>/incident.json` 与此前的 `attempts/<n>.md`；
   - 先运行一次 `recovery_check`，它给出位置检查、类型错误与试运行的 stderr。
3. 找到出错的文件：stderr 中的路径与行号、类型错误的位置、最近一次应用改动的文件（`recovery_snapshot diff`，或下面第 6 节的 git 命令）。阅读 `workspace/AGENTS.md` 中这个扩展的登记，了解它的用途与依赖。
4. 修复，按以下顺序选择做法：
   1. 原因清楚、能直接修正，并且不影响扩展原有的功能时，直接修改文件；
   2. 最近一次改动把原本正常的扩展改坏了，又无法修正时，用 `recovery_snapshot restore`，`ref` 取这次改动之前的快照（例如 `apply/<n>^` 或 `last-good`），`paths` 只列出出错的文件，让它回到之前能正常工作的版本；
   3. 两种做法都不行时，把出错的扩展（单个文件或整个扩展目录）移到同一个扩展目录下的 `.disabled/`，例如 `workspace/.pi/extensions/weather.ts` 移到 `workspace/.pi/extensions/.disabled/weather.ts`。pi 与类型检查都跳过以 `.` 开头的条目。其他扩展依赖它（例如通过 `pi.events` 上的 `<扩展名>:<事件>` 调用它）时，一并检查它们。
   位置检查失败时，按第 5 节把 profile 中的内容移到工作区。
5. 执行 `recovery_check`。不通过时根据报告继续修改，直到通过。
6. 执行 `recovery_report`。

## 5. 常见原因与修复方式

### 加载失败（进程以退出码 1 退出，stdout 没有任何记录）

stderr 中是 pi 的加载错误，最后一行总是 `Hint: Start without extensions using "pi -ne".`：

| stderr | 原因 | 修复 |
|---|---|---|
| `Error: Failed to load extension "<路径>": Failed to load extension: ParseError: Unexpected token …`，下一行是 `<文件>:<行>:<列>` | 语法错误 | 修正该位置 |
| `Error: Failed to load extension "<路径>": Failed to load extension: <消息>` | 工厂函数（默认导出的函数）抛出了异常，或者模块顶层代码抛出了异常 | 根据消息修正；工厂函数中不要做可能失败的外部操作 |
| `Extension does not export a valid factory function: <路径>` | 没有默认导出的函数 | 加上 `export default function (pi: ExtensionAPI) { … }` |
| `Tool "<名称>" conflicts with <另一个扩展的路径>` | 两个扩展注册了同名工具 | 改名其中一个，并同步修改引用它的提示、技能与 `AGENTS.md` 登记 |
| `Flag "--<名称>" conflicts with <路径>` | 两个扩展注册了同名 flag | 改名其中一个 |
| `Tool "<名称>" registered by extension "<路径>" must define an object parameter schema.` | 工具的 `parameters` 不是对象 schema | 用 `Type.Object({...})` |
| `Cannot find module '<名称>'` | 扩展依赖的 npm 包没有安装，或者导入了 pi 不提供的子路径 | 在扩展目录执行 `npm install`（`package.json` 中写明依赖）；pi 提供的模块见 `bearbot-extension-dev` 技能 |
| `Extension path does not exist: <路径>` | 设置中引用了不存在的扩展路径 | 修正或删除这一项 |

扩展目录的加载规则：`.pi/extensions/` 下第一层的 `.ts`、`.js` 文件直接加载；子目录中有 `package.json` 且 `pi.extensions` 列出了存在的文件时加载这些文件，否则加载 `index.ts`，再否则加载 `index.js`，都没有时整个子目录不加载。以 `.` 开头的条目、`node_modules`、被该目录中 `.gitignore`、`.ignore`、`.fdignore` 忽略的条目都跳过。`.pi/extensions/` 本身直接有 `index.ts`、`index.js` 或带 `pi.extensions` 的 `package.json` 时，只加载它们，目录中的其他扩展都不加载。

### 运行一段时间后退出

| 现象 | 原因 | 修复 |
|---|---|---|
| stderr 中是 Node 的默认错误输出：`<文件>:<行>`、出错的代码行、`Error: …`、回溯信息，最后一行 `Node.js v<版本>`；`exits` 中 `code` 为 1 | 扩展在定时器、socket 或 HTTP 服务回调、事件监听函数中抛出了异常，或者有未处理的 Promise 拒绝。pi 的 RPC 模式没有 `uncaughtException`、`unhandledRejection` 处理，这类异常会终止整个进程 | 在这些回调中用 `try/catch` 处理异常并通过 bearbot SDK 的 `diagnostic()` 报告；没有 `await` 的 Promise 加上 `.catch(...)` |
| 扩展调用了 `process.exit()` 或 `ctx.shutdown()` | 扩展主动结束了进程 | 删除这些调用 |
| `exits` 中 `signal` 为 `SIGKILL` 或 `SIGTERM`，daemon 日志中有心跳超时 | 同步代码占住了 Node 事件循环（例如死循环、同步执行很久的命令），`get_state` 在 `rpc_timeout` 内没有响应，daemon 终止了进程树 | 找到并去掉阻塞事件循环的同步代码 |

`rpc` 中的 `extension_error` 是扩展事件处理函数抛出的异常，pi 捕获了它们，它们本身不会结束进程，但通常指向同一个有问题的扩展。

### 启动超时

| 现象 | 原因 | 修复 |
|---|---|---|
| 故障报告或 daemon 日志说明 `get_state` 在 `start_timeout` 内没有响应 | 某个扩展的工厂函数或 `session_start` 处理函数一直没有返回（例如等待一个不会完成的网络连接）。pi 等全部工厂函数、`session_start` 与 `resources_discover` 处理函数完成后才响应 `get_state` | 不要在这些地方等待外部服务；长期运行的连接在 `session_start` 中以不等待的方式开始 |
| 故障报告或 daemon 日志说明 `bearbot-im` 在 `ready_timeout` 内没有调用 `bridge/ready` | `bearbot-im` 没能完成与 gateway 的 `hello`。gateway 日志中能看到原因，例如已有另一条连接占用了 `gateway.sock`（脱离进程树的后台进程） | 工作区中的改动无法修复这种情况时报告 `cannot_fix`，在 `summary` 中写明 gateway 日志中的原因 |

### 位置检查失败

把 profile 中的内容移到工作区的对应位置，然后删除 profile 中的原文件：

| profile 中的位置 | 移到 |
|---|---|
| `profile/agent/extensions/<条目>` | `workspace/.pi/extensions/<条目>` |
| `profile/agent/skills/<条目>` | `workspace/.pi/skills/<条目>` |
| `profile/agent/prompts/<条目>` | `workspace/.pi/prompts/<条目>` |
| `profile/agent/mcp.json` | `workspace/.pi/mcp.json`；工作区已有这个文件时把 `mcpServers` 中的条目合并进去，同名条目保留工作区的版本并在报告中说明 |
| `profile/agent/AGENTS.md`、`AGENTS.override.md`、`AGENTS.MD`、`CLAUDE.md`、`CLAUDE.MD` | 把内容合并进 `workspace/AGENTS.md` 的对应小节，然后从 `profile/agent/` 中删除这些文件 |
| `profile/agent/SYSTEM.md`、`profile/agent/APPEND_SYSTEM.md` | `workspace/.pi/SYSTEM.md`、`workspace/.pi/APPEND_SYSTEM.md`；工作区已有时合并内容 |

移过去的扩展会进入类型检查与试运行，可能暴露新的错误，按上面的方法继续修复。空的 `extensions/`、`skills/`、`prompts/` 目录不影响检查。

### 不会导致启动失败的问题

`workspace/.pi/settings.json`、`profile/agent/settings.json` 的 JSON 错误只在 stderr 中产生 `Warning: Invalid settings file …`，`mcp.json` 的错误与 MCP 服务器连接失败只产生 `MCP servers need attention: …` 警告，进程照常启动。故障的原因不在这里时，这些问题可以一并修正，并在报告中说明。

## 6. 快照仓库

快照仓库属于 root，你可以读取，不能写入：`commit`、`restore`、`checkout`、`reset`、`tag` 等写入命令会因为权限失败。恢复文件用 `recovery_snapshot restore`。

```sh
G="git --git-dir=/bearbot/profile/bearbot/snapshots.git --work-tree=/bearbot"
$G log --oneline --decorate -n 30                      # 快照历史，引用名显示在括号中
$G log -n 5 --stat                                     # 每次提交的说明（摘要、发起者、发起的会话串）与改动的文件
$G show --stat apply/12                                # 一次应用改动了哪些文件
$G diff apply/12^ apply/12 -- workspace/.pi            # 一次应用的完整改动
$G diff last-good -- workspace/.pi profile/agent       # 当前文件与 last-good 的差别
$G diff incident/<故障id>/before -- workspace/.pi      # 你到目前为止的修改
$G show last-good:workspace/.pi/extensions/weather.ts  # 取出旧版本的内容
```

快照只包含 `workspace/AGENTS.md`、`workspace/CLASSIFIER.md`、`workspace/.pi/**`、`profile/agent/settings.json`、`profile/agent/models.json`，不包含 `node_modules/`、`*.db`、`*.log`。历史是一条直线，任一提交的父提交就是它之前的状态。

## 7. 报告

`recovery_report` 的参数：

| 字段 | 内容 |
|---|---|
| `status` | `fixed`：已经修复，最后一次 `recovery_check` 通过；`cannot_fix`：无法修复 |
| `summary` | 故障的原因与所做的修改，写给管理员与 agent 看。移到 `.disabled/` 或从快照恢复的，写明涉及的扩展与失去的功能。`cannot_fix` 时写明原因、已经尝试过的做法，以及管理员可以采取的处理（例如回滚到哪个快照） |
| `changed_files` | 修改、新建、删除、移动、恢复过的文件的绝对路径，移动的文件同时列出原位置与新位置；没有改动时为空数组 |
| `lesson` | 写给 agent 看的经验：错误的原因，以及以后编写扩展时怎样避免。agent 会把它写入记忆的“经验记录” |

`fixed` 的例子：

```json
{
  "status": "fixed",
  "summary": "weather 扩展在 setInterval 回调中直接访问 response.data.temp，接口返回错误时 data 为 undefined，抛出的 TypeError 没有被捕获，使 agent 进程退出。已在回调中加上 try/catch，失败时通过 diagnostic() 报告，功能保持不变。recovery_check 通过。",
  "changed_files": ["/bearbot/workspace/.pi/extensions/weather/index.ts"],
  "lesson": "定时器回调中抛出的异常会终止整个 agent 进程。定时器、socket 回调与没有 await 的 Promise 都要自己捕获异常，并用 bearbot.diagnostic() 报告。"
}
```

`cannot_fix` 的例子：

```json
{
  "status": "cannot_fix",
  "summary": "bearbot-im 在 ready_timeout 内没有完成与 gateway 的 hello，gateway 日志显示已有另一条连接占用 gateway.sock。工作区中的扩展都能正常加载，recovery_check 通过，原因不在工作区。建议管理员检查容器中脱离进程树的后台进程，必要时重启容器。",
  "changed_files": [],
  "lesson": "不要在扩展或 bash 中启动会连接 /run/bearbot/ 下 socket 的后台进程。"
}
```

提交报告后本轮结束，不要再修改任何文件。
