# bearbot 设计文档

## 1. 名词

| 名词 | 含义 |
|---|---|
| `bearbotd` | Go 编写的 daemon，容器入口程序，负责拉起、监视、修复其余组件，并提供命令行界面 |
| harness 组件 | `pi-acp` 进程，以及它为每个 ACP 会话拉起的 `pi --mode rpc` 子进程 |
| nonebot 组件 | 运行 nonebot2 的 Python 进程，内含对接插件 `nonebot_plugin_bearbot`（下称 bridge） |
| 工作区 | `/bearbot/workspaces/<id>/` 目录，pi 会话的 cwd，内含 `AGENTS.md` 记忆、`.pi/` 配置和 agent 产生的文件 |
| 场景 | nonebot 中的一个聊天上下文（私聊、群聊、频道子频道），由 `nonebot-plugin-uninfo` 的会话信息确定 |
| 绑定 | 场景到工作区的关联，即需求中的“attach 当前聊天上下文到 namespace”，namespace 即工作区 id |
| 信封 | bridge 发给 pi 的结构化提示词（第 8 节），携带发送者、场景、权限信息 |
| 故障 | 任一组件异常退出或健康检查失败后，daemon 建立的一条处理记录（incident） |
| 主修复 | 用正常 profile 的 harness 修复故障 |
| recovery 修复 | 用独立的 recovery profile 拉起的 pi 修复故障 |
| 维护模式 | recovery 修复也无法进行时 daemon 进入的等待人工处理状态 |

## 2. 调研结论

所有结论来自对已发布软件包的源码和自带文档的阅读，版本以 2026-10-26 的 npm / PyPI / Go proxy 为准。

### 2.1 pi（`@earendil-works/pi-coding-agent` 1.4.0）

原包名 `@mariozechner/pi-coding-agent` 已标记弃用，后续统一使用 `@earendil-works/pi-coding-agent`。要求 Node.js ≥ 22.19。

| 事实 | 对设计的作用 |
|---|---|
| `PI_CODING_AGENT_DIR` 指定全局配置目录（默认 `~/.pi/agent`），其中有 `auth.json`、`settings.json`、`mcp.json`、`models.json`、`AGENTS.md`、`extensions/`、`skills/`、`prompts/`、`sessions/` | 全局 profile 直接用这个目录，全部 harness 共享 |
| 全局 `AGENTS.md` 对所有工作目录生效；cwd 及其各级父目录中的 `AGENTS.md` 也会加载，加载不需要项目信任 | 全局记忆放在 profile 的 `agent/AGENTS.md`，工作区记忆放在工作区根目录的 `AGENTS.md` |
| 项目级 `.pi/settings.json`、`.pi/mcp.json`、`.pi/extensions/`、`.pi/skills/`、`.pi/prompts/`、`.pi/SYSTEM.md`、`.pi/APPEND_SYSTEM.md` 以及 `.agents/skills/` 只有在授予项目信任后才加载 | 工作区可以带自己的 MCP、技能、扩展、提示词模板和模型设置 |
| 项目信任的判定顺序：命令行 `--approve/--no-approve` → 全局扩展或命令行扩展处理 `project_trust` 事件 → `trust.json` → `defaultProjectTrust`。RPC 模式无法弹出确认 | 核心扩展通过 `-e` 加载，处理 `project_trust`：cwd 位于工作区父目录内时返回 `yes`，其余返回 `no` |
| MCP 已内置（`builtin:mcp`），支持 stdio 与 streamable HTTP，项目条目覆盖同名全局条目；扩展可调用 `pi.registerMcpServer()` | 工作区 MCP 用 `.pi/mcp.json`，不需要额外的 MCP 扩展 |
| 扩展是 TypeScript 模块，由 jiti 直接加载；`typebox` 与 pi 自身的包由加载器提供别名，其余依赖放在扩展旁的 `package.json` | 核心扩展和 agent 编写的扩展都无需编译 |
| 扩展事件：`input` 可改写或拦截输入；`before_agent_start` 可修改 `systemPromptOptions`（含 `contextFiles`、`sections`）；`tool_call` 可阻止工具调用，codemode 脚本发起的嵌套调用同样经过它；`session_compact` 在压缩成功后触发；`pi.sendMessage(..., { deliverAs: "steer" })` 可在运行中插入消息；`ctx.isIdle()` 判断是否空闲 | 发送者感知、权限控制、压缩后重新加载 `AGENTS.md` 都在核心扩展里实现 |
| RPC 模式下 stdout 只输出协议记录，诊断信息写到 stderr；以 `/` 开头的提示词若是扩展命令会立即执行 | shim 需要单独保存 stderr；信封以 `@bearbot/v1` 开头，聊天文本不会被当作命令执行 |
| 运行事件流包含 `agent_start`、`agent_end`、`agent_settled`，助手消息带 `stopReason`（含 `error`）和 `errorMessage` | shim 据此判断 pi 是否空闲、本轮是否因模型服务出错而结束 |
| CLI 参数：`-ne`（禁用扩展，`-e` 显式加载的仍生效）、`-ns`、`-np`、`-nc`、`-na`、`--skill`、`--append-system-prompt`、`--session-dir`、`--no-session` | recovery 修复用这些参数拉起只含最少资源的 pi |

### 2.2 pi-acp 0.0.34

| 事实 | 对设计的作用 |
|---|---|
| 通过 stdio 与一个 ACP 客户端通信，每个 ACP 会话拉起一个 `pi --mode rpc --no-themes [--session <file>]` 子进程，cwd 为 `session/new` 的 `cwd` | 一个 harness 进程服务所有工作区；会话 cwd 就是工作区目录 |
| 子进程命令可由环境变量 `PI_ACP_PI_COMMAND` 替换，子进程继承 pi-acp 的环境变量 | 用 `bearbot-pi` 包装脚本插入 shim，追加核心扩展、技能参数，并收集 stderr 与退出状态 |
| 子进程的 stderr 被直接丢弃；子进程退出时只让挂起的请求失败，pi-acp 本身继续运行 | daemon 无法从 pi-acp 得知 pi 子进程的故障，必须由 shim 上报 |
| 会话映射保存在 `$HOME/.pi/pi-acp/session-map.json`（固定使用 home 目录）；`session/load` 会关闭旧子进程并按会话文件重新拉起 | 把 profile 挂载点链接到 `$HOME/.pi`；`session/load` 同时用作“重新加载会话”的手段 |
| 没有空闲回收，也没有关闭会话的接口 | 空闲回收由 shim 完成 |
| `prompt` 中以 `/` 开头的文本，`compact`、`session`、`name`、`autocompact` 等由 pi-acp 自行处理，其余交给 pi | 管理命令 `/bb compact` 直接发送 `/compact` |
| 不转发 ACP `session/new` 的 `mcpServers`，不支持 ACP 的 `fs/*`、`terminal/*`，`resource` 块被转成纯文本 | 工作区 MCP 只走 `.pi/mcp.json`；客户端能力声明为不支持文件系统与终端 |
| 未设置 `quietStartup` 时会在会话中输出启动信息；会检查 npm 上的新版本 | 全局 settings 默认写入 `quietStartup: true`，环境变量设置 `PI_SKIP_VERSION_CHECK=1`；bridge 只收集请求进行中的更新 |

### 2.3 ACP SDK

| 软件包 | 事实 |
|---|---|
| Go：`github.com/coder/acp-go-sdk` v0.13.5 | `NewClientSideConnection(client, w, r)`；实现 `ExtensionMethodHandler` 即可收发 `_` 开头的扩展方法 |
| Python：`agent-client-protocol` 0.12.1 | `connect_to_agent(client, writer, reader)` 接受任意 asyncio 字节流（可用 Unix socket）；`Client` 接口带 `ext_method` / `ext_notification` |

### 2.4 nonebot2 生态

| 软件包 | 版本 | 用途 |
|---|---|---|
| `nonebot2` | 2.5.0 | `nonebot.message.handle_event(bot, event)` 分发自定义事件，按优先级依次 await 全部匹配器；匹配器只在 `matcher.type` 为空或等于 `event.get_type()` 时响应 |
| `nonebot-plugin-uninfo` | 0.11.2 | 跨适配器的 `Session`（`scope`、`self_id`、`scene`、`user`、`member`、`scene_path`） |
| `nonebot-plugin-alconna` | 0.62.1 | 命令解析、`UniMessage` 跨平台消息、`Target.dump()/load()` 保存主动发送目标 |
| `nonebot-plugin-localstore` | 0.7.4 | 插件数据目录 |
| `nonebot-plugin-orm` | 0.8.4 | SQLAlchemy + Alembic，bridge 数据库 |
| `nonebot-plugin-apscheduler` | — | 预装，agent 可编写定时任务插件 |

## 3. 进程结构

```
tini (PID 1)
└── bearbotd  (root)
    ├── [harness]  pi-acp                         (uid bearbot, 独立进程组)
    │     ├── bearbot-pi → bearbotd pi-shim        (每个 ACP 会话一个)
    │     │     └── pi --mode rpc -e core ...      (cwd = 工作区)
    │     │           └── MCP stdio 服务器 ...
    │     └── ...
    ├── [nonebot]  python bot.py                   (uid bearbot, 独立进程组)
    └── [recovery] pi --mode rpc (recovery profile) (仅在 recovery 修复期间存在)

Unix socket（/run/bearbot/）
    acp.sock    ACP 路由入口，bridge 连接（组 bearbot 可读写）
    agent.sock  agent API，核心扩展与 shim 调用（组 bearbot 可读写）
    admin.sock  管理接口，`bearbotd ctl` 使用（仅 root）
```

数据流：

```
IM 平台 ⇄ nonebot 适配器 ⇄ bridge ──ACP(acp.sock)──▶ bearbotd ACP 路由 ──stdio──▶ pi-acp ──RPC──▶ pi(工作区)
                                    ◀─_bearbot/* 扩展方法─┘                                        │
                                                                                                    │ 核心扩展工具
bearbotd agent API (agent.sock) ◀───────────────────────────────────────────────────────────────────┘
        └─ im 发送、插件动作 → 经 ACP 连接上的 _bearbot/* 方法转给 bridge
```

PID 1 使用 tini：pi-acp 退出后，pi 子进程和 MCP 服务器会被过继给 PID 1，需要有进程回收它们；Go 的 `os/exec` 依赖 `Wait` 取得子进程退出状态，daemon 自己调用 `wait(-1)` 回收孤儿进程会抢走这些状态，所以回收工作交给 tini。

## 4. 容器目录与挂载点

### 4.1 挂载点

| 挂载点 | 内容 | 所有者 |
|---|---|---|
| `/bearbot/profile` | 全局 pi profile、pi-acp 会话映射、daemon 状态 | bearbot（其中 `bearbotd/` 子目录为 root） |
| `/bearbot/workspaces` | 工作区父目录 | bearbot |
| `/bearbot/nonebot` | nonebot 扩展插件、Python 依赖、插件数据 | bearbot |
| `/bearbot/config` | `bearbot.toml`、recovery profile 模板、维护脚本 | root，权限 0700 |

镜像内 `/home/bearbot/.pi` 是指向 `/bearbot/profile` 的符号链接，`PI_CODING_AGENT_DIR=/bearbot/profile/agent`，因此 pi 与 pi-acp 的全部持久数据都落在 profile 挂载点。

### 4.2 profile 挂载点

```
/bearbot/profile/
├── agent/                      PI_CODING_AGENT_DIR
│   ├── auth.json               模型服务凭据（pi /login 写入）
│   ├── settings.json           首次启动时由 daemon 写入默认值
│   ├── mcp.json  models.json
│   ├── AGENTS.md               全局记忆
│   ├── extensions/  skills/  prompts/
│   └── sessions/               pi 会话文件
├── pi-acp/session-map.json
├── bin/                        agent 安装的持久化可执行文件，位于 PATH 最前
└── bearbotd/                   root 所有，bearbot 只读
    ├── logs/                   daemon.log harness.log nonebot.log pi/<会话id>.log
    ├── incidents/<id>/         report.md、各次修复记录、会话文件路径
    ├── recovery-sessions/      recovery 修复的 pi 会话文件
    ├── snapshots.git/          代码快照仓库（第 6.8 节）
    └── state.json              维护模式标记、故障计数
```

daemon 首次写入的 `agent/settings.json`：

```json
{
  "quietStartup": true,
  "defaultProjectTrust": "never",
  "enableInstallTelemetry": false,
  "compaction": { "enabled": true }
}
```

`defaultProjectTrust: "never"` 保证核心扩展未加载时任何目录都不会被信任；工作区的信任只由核心扩展授予。

### 4.3 工作区挂载点

```
/bearbot/workspaces/
├── _system/                    保留工作区，修复会话与健康探测使用
│   └── AGENTS.md               修复经验记忆
├── <id>/
│   ├── AGENTS.md               工作区记忆
│   ├── .pi/
│   │   ├── settings.json       可选：defaultModel、defaultTools 等
│   │   ├── mcp.json            工作区 MCP 服务器
│   │   ├── skills/             工作区技能
│   │   ├── extensions/         工作区 pi 扩展
│   │   └── prompts/
│   ├── docs/                   约定的资料目录
│   └── inbox/<日期>/           聊天中收到的文件
└── .archive/<id>-<时间>/       归档的工作区
```

工作区 id 匹配 `^[a-z0-9][a-z0-9-]{0,62}$`，下划线开头的名字保留给系统。

### 4.4 nonebot 挂载点

```
/bearbot/nonebot/
├── plugins/                    agent 编写的 nonebot 插件，bot.py 用 nonebot.load_plugins 加载
├── site-packages/              agent 安装的 Python 依赖（uv pip install --target）
├── data/  cache/  config/      nonebot-plugin-localstore 目录
│   └── data/nonebot_plugin_bearbot/bearbot.db
└── errors.jsonl                插件运行时异常记录（第 9.8 节）
```

### 4.5 配置挂载点

```
/bearbot/config/
├── bearbot.toml
├── recovery/                   recovery profile 模板
│   ├── settings.json
│   ├── auth.json               可选，也可在 bearbot.toml 中用环境变量提供 API key
│   └── models.json             可选
└── hooks/                      维护模式脚本（可选）
```

### 4.6 镜像内只读部分

```
/opt/bearbot/
├── venv/                       nonebot 运行环境（含 bridge）
├── nonebot/bot.py
├── pi-extension/core/          核心扩展
├── pi-extension/recovery/      recovery 扩展
└── skills/                     bearbot 自带技能
/usr/local/bin/bearbotd
/usr/local/bin/bearbot-pi       exec /usr/local/bin/bearbotd pi-shim -- "$@"
```

## 5. 用户与权限

- `bearbotd` 以 root 运行；harness、nonebot、recovery 进程通过 `SysProcAttr.Credential` 以 `bearbot` 用户运行。
- `/opt/bearbot`、`/usr/local/bin` 归 root 所有且对 bearbot 只读。agent 因此无法修改 bridge、核心扩展、daemon，这就是“agent 不允许直接修改对接插件源码”的实现方式。bridge 的升级只能通过重建镜像。
- `/bearbot/config` 权限 0700。nonebot 需要的适配器令牌等配置由 daemon 读出后以环境变量传入；recovery 的凭据只在 recovery 修复期间复制到 `/run/bearbot/recovery-agent/`。
- `admin.sock` 仅 root 可用，提供生成管理员令牌、退出维护模式等操作；`agent.sock` 只提供第 6.10 节列出的操作。
- 挂载点的属主不符合要求时 daemon 打印需要执行的 `chown` 命令并以非零状态退出。镜像构建参数 `BEARBOT_UID`、`BEARBOT_GID` 用来匹配宿主机目录的属主。

## 6. bearbotd

### 6.1 Go 包结构

```
daemon/
├── cmd/bearbotd/main.go        cobra 根命令：run、ctl、pi-shim
└── internal/
    ├── config/                 解析并校验 bearbot.toml（go-toml/v2），错误即退出
    ├── layout/                 挂载点检查、目录与默认文件初始化
    ├── proc/                   以 bearbot 身份启动进程、独立进程组、收集子孙进程树、分级终止、日志写入（lumberjack 轮转 + 内存环形缓冲）
    ├── supervisor/             组件状态机、就绪与健康判定、重启
    ├── incident/               故障记录、修复调度、升级规则、维护模式
    ├── acprouter/              ACP 多路复用、会话归属、_bearbot/* 扩展方法
    ├── repair/                 主修复（acp-go-sdk 客户端，经内存管道接入路由）与 recovery 修复
    ├── pirpc/                  pi RPC JSONL 客户端（recovery 修复、pi 检查使用）
    ├── shim/                   pi-shim 子命令
    ├── snapshot/               调用 git 命令行维护快照
    ├── agentapi/               agent.sock 上的 HTTP 接口
    └── console/                交互界面（reeflective/console）与 admin.sock 远程执行
```

依赖：`github.com/spf13/cobra`、`github.com/reeflective/console`、`github.com/pelletier/go-toml/v2`、`gopkg.in/natefinch/lumberjack.v2`、`github.com/coder/acp-go-sdk`、`golang.org/x/sys/unix`、`golang.org/x/term`。

### 6.2 启动顺序

1. 读取 `/bearbot/config/bearbot.toml` 并校验，失败即退出。
2. 检查四个挂载点的属主与可写性；创建缺失目录；首次运行时写入默认 `settings.json`、`_system/AGENTS.md`。
3. 初始化快照仓库。
4. 读取 `state.json`：若上次退出时处于维护模式，直接进入维护模式并等待人工处理。
5. 创建三个 socket。
6. 若 `[nonebot] packages` 相对上次安装有变化，执行 `uv pip install --target /bearbot/nonebot/site-packages`。
7. 启动 harness：拉起 pi-acp，路由完成 `initialize`，执行健康探测（第 6.5 节）。
8. 启动 nonebot，等待 bridge 发送 `_bearbot/bridge/hello`。
9. 两个组件都在 `stable_after` 内保持健康后，给当前快照打 `last-good` 标签。
10. 启动交互界面。

第 7、8 步任一失败都会建立故障并进入第 6.6 节的流程。

收到 SIGTERM 时：通知 bridge 停止接收新消息 → 等待进行中的回合结束，最长 `shutdown_grace` → 依次终止 nonebot、harness。建议 `docker run --stop-timeout 90`。

### 6.3 ACP 路由

pi-acp 只能服务一个 stdio 客户端，而 bridge、daemon 的修复与探测、交互界面里的 `chat` 都需要和它通信。daemon 持有 pi-acp 的 stdio，在 `acp.sock` 上为每个连接扮演 ACP agent，自身内部的客户端通过 `net.Pipe` 以同样方式接入。路由使用换行分隔的 JSON-RPC 2.0 帧。

| 消息 | 路由规则 |
|---|---|
| `initialize`（客户端发出） | harness 启动时路由以客户端能力 `fs: false, terminal: false` 向 pi-acp 发送一次，之后对每个客户端直接返回缓存结果 |
| 客户端请求 | 重写为路由自己的递增 id 后转发，响应按原 id 返回给发起者 |
| `session/new`、`session/load` 的成功响应 | 记录会话 id 的归属为发起者 |
| 客户端通知（`session/cancel`） | 直接转发 |
| pi-acp 的 `session/update` 通知 | 按 `sessionId` 发给归属者；归属者已断开时存入该会话的暂存区 |
| pi-acp 发往客户端的请求（如 `session/request_permission`） | 按 `sessionId` 发给归属者；无归属者时返回错误 |
| `_bearbot/*` 方法 | 由路由自身处理，不转发 |

暂存区与重新接管：nonebot 重启期间完成的回合，其输出留在暂存区。bridge 重新连接后对自己记录的每个会话调用 `_bearbot/session/attach {sessionId}`，路由把归属改为该连接，并在响应中返回暂存的更新与回合结束结果，bridge 照常发送到聊天中。

harness 重启：路由让所有挂起请求以错误 `harness restarted` 结束，清空归属表，向所有连接发送 `_bearbot/harness/state {state}`。bridge 收到 `up` 后把全部会话标为“下次使用前需要 `session/load`”。

`_bearbot/*` 方法一览：

| 方向 | 方法 | 用途 |
|---|---|---|
| bridge → daemon | `_bearbot/bridge/hello` | 就绪握手，携带已加载与加载失败的插件清单 |
| bridge → daemon | `_bearbot/session/attach` | 重新接管会话 |
| bridge → daemon | `_bearbot/status` | `/bb status` 查询组件状态 |
| bridge → daemon | `_bearbot/restart` | `/bb restart` |
| daemon → bridge | `_bearbot/admin/createToken` | 生成一次性管理员令牌 |
| daemon → bridge | `_bearbot/admin/list`、`_bearbot/admin/revoke` | 管理员管理 |
| daemon → bridge | `_bearbot/im/send`、`_bearbot/im/actions`、`_bearbot/im/call` | agent 工具调用的转发 |
| daemon → bridge | `_bearbot/session/reload` | 回合结束后对指定会话执行 `session/load` |
| daemon → bridge | `_bearbot/session/evicted` | 通知会话的 pi 进程已被空闲回收 |
| daemon → bridge | `_bearbot/harness/state` | harness 状态变化 |
| daemon → bridge | `_bearbot/notify` | 故障与维护通知，发给管理员 |
| daemon → bridge | `_bearbot/drain` | 停止接收新消息（关闭容器前） |

### 6.4 pi-shim

pi-acp 通过 `PI_ACP_PI_COMMAND=/usr/local/bin/bearbot-pi` 拉起 shim，shim 再拉起真正的 pi：

```
pi -e /opt/bearbot/pi-extension/core --skill /opt/bearbot/skills <pi-acp 传入的参数>
```

shim 的职责：

1. 双向复制 stdin/stdout，并逐行解析（只按 LF 分帧，与 pi 文档要求一致）：
   - 从 pi-acp 发出的 `get_state` 的响应中取得 `sessionId`、`sessionFile`；
   - 从 `agent_start` / `agent_settled` 判断是否空闲；
   - 从 `agent_end` 中助手消息的 `stopReason: "error"` 与 `errorMessage` 取得本轮的模型服务错误。
2. stderr 写入 `/bearbot/profile/bearbotd/logs/pi/<sessionId>.log`（取得 id 前先写入以 PID 命名的文件，之后改名），同时保留最后 200 行。
3. 启动、每轮结束、退出时调用 `agent.sock` 的 `/v1/internal/shim/*` 上报：PID、cwd、会话 id、本轮结果、退出码、信号、stderr 末尾。
4. 空闲回收：空闲超过 `harness.idle_evict_after` 且没有新的 stdin 输入时，关闭 pi 的 stdin，等待退出，上报 `evicted`，然后以 0 退出。daemon 随即向会话归属者发送 `_bearbot/session/evicted`。
5. 收到 SIGTERM（pi-acp 在 `session/load` 或退出时发出）时转发给 pi，上报 `terminated_by_parent`。

daemon 对 shim 退出上报的处理：`evicted`、`terminated_by_parent` 视为正常；其余非零退出或信号退出视为 harness 侧故障，故障范围标为对应的工作区。

### 6.5 健康判定

| 组件 | 就绪条件 | 健康条件 |
|---|---|---|
| harness | pi-acp 进程存在，路由 `initialize` 成功 | 健康探测成功：对 `_system` 工作区的探测会话执行 `session/load`（首次为 `session/new`，会话 id 记在 `state.json`），`probe_timeout` 内返回成功且 shim 上报 pi 进程仍在运行。`session/load` 会拉起新的 pi 进程，加载全局扩展、全局 settings 和 `_system` 的资源，所以这一步能发现全局配置损坏 |
| nonebot | 进程存在，`ready_timeout` 内收到 `_bearbot/bridge/hello` | 进程存在，ACP 连接未断开 |

“harness 可用”指 harness 健康且最近一次健康探测成功。harness 启动后、每次修复完成后、每次 shim 上报故障后都会重新探测。

### 6.6 故障处理

#### 触发条件

| 事件 | 故障类型 |
|---|---|
| nonebot 进程以非零状态或信号退出，或在非 daemon 发起的情况下以 0 退出 | `nonebot_exit` |
| nonebot 在 `ready_timeout` 内没有完成握手 | `nonebot_not_ready` |
| `hello` 中有加载失败的插件 | `nonebot_plugin_load` |
| pi-acp 进程退出 | `harness_exit` |
| 健康探测失败 | `harness_unhealthy` |
| shim 上报 pi 进程异常退出 | `pi_session_exit`（带工作区） |

同一时间只存在一个未关闭的故障，新事件追加到已有故障中。

#### 状态机

```
            ┌──────────────────────────────────────────────────────────────┐
            ▼                                                              │
 RUNNING ──事件──▶ INCIDENT ──harness 可用──▶ REPAIR_MAIN ──报告 fixed/no_change──▶ VERIFY ──通过──┘
                       │                         │   ▲                                  │
                       │                         │   └──未通过且主修复次数 < main_attempts─┘
                       │                         │
                       │      报告 cannot_fix / 无报告 / 超时 / 本轮模型服务错误 /
                       │      harness 在修复中失效 / 主修复次数用尽
                       │                         ▼
                       └──harness 不可用──▶ REPAIR_RECOVERY ──报告 fixed/no_change──▶ VERIFY ──通过──▶ RUNNING
                                                 │   ▲                                  │
                                                 │   └──未通过且 recovery 次数 < recovery_attempts
                                                 │
                         recovery 拉起失败 / 模型服务错误 / cannot_fix / 次数用尽
                                                 ▼
                                            MAINTENANCE
```

- 进入 `INCIDENT` 时：停止自动重启出故障的组件，提交一次快照（`incident/<id>/before`），记录事件、退出状态、日志末尾。
- `VERIFY`：按故障类型重启相关组件（`harness_*`、`pi_session_exit` 重启 harness 或只做健康探测，`nonebot_*` 重启 nonebot），之后要求全部组件在 `stable_after` 内保持健康，且 `hello` 中没有加载失败的插件。通过后关闭故障，提交快照并移动 `last-good` 标签，通过 bridge 通知管理员。
- 每次修复结束都提交快照（`incident/<id>/attempt-<n>`），下一次修复的提示中带上前几次的报告与差异。
- harness 本身出故障时 harness 不可用，直接进入 `REPAIR_RECOVERY`。

#### 主修复

1. 路由内部客户端对 `/bearbot/workspaces/_system` 执行 `session/new`，每个故障使用新会话。
2. 发送 `kind: "system"`、`role: "system"` 的信封，`incident` 字段包含：故障 id、类型、组件、退出码与信号、相关日志末尾 200 行、完整日志路径、`git diff --stat last-good` 结果、快照仓库的读取方式、此前各次修复的报告。
3. 等待 agent 调用 `bearbot_repair_report`（经 `agent.sock` 送达 daemon），上限 `repair_timeout`。
4. 回合结束而没有报告时，若 shim 上报了本轮的模型服务错误，立即升级到 recovery 修复；否则记为一次失败的主修复。

#### recovery 修复

1. 清空并重建 `/run/bearbot/recovery-agent/`（tmpfs），复制 `/bearbot/config/recovery/` 中的文件，属主改为 bearbot。每次 recovery 都从模板重建，主 agent 对 profile 的任何改动都不会影响它。
2. 以 bearbot 身份、cwd `/bearbot` 拉起：

   ```
   PI_CODING_AGENT_DIR=/run/bearbot/recovery-agent \
   pi --mode rpc -ne -e /opt/bearbot/pi-extension/recovery \
      -ns --skill /opt/bearbot/skills/bearbot-repair -np -nc -na \
      --session-dir /bearbot/profile/bearbotd/recovery-sessions
   ```

   recovery 扩展只注册 `bearbot_repair_report`、`bearbot_logs`、`bearbot_status`、`bearbot_nonebot_check`、`bearbot_pi_check` 这几个工具，并在系统提示中加入 recovery 规则。
3. daemon 用 `pirpc` 发送同样内容的信封，消费事件直到 `agent_settled`。
4. 以下情况都判定为“recovery 拉起失败”，直接进入维护模式：进程在 `agent_settled` 前退出；`prompt` 响应 `success: false`；回合以 `stopReason: "error"` 结束（额度耗尽、凭据失效等）；`[recovery]` 未配置可用凭据（启动前用 `pi auth check` 检查）。

#### 维护模式

1. 把维护标记与原因写入 `state.json`，容器重启后仍保持维护模式。
2. 以 root 执行 `[maintenance] on_enter` 中的命令，超时 `hook_timeout`。环境变量：`BEARBOT_EVENT=maintenance_enter`、`BEARBOT_INCIDENT_ID`、`BEARBOT_REASON`、`BEARBOT_REPORT=/bearbot/profile/bearbotd/incidents/<id>/report.md`。命令退出码非零时写入日志并在交互界面显示。
3. 不再自动重启或修复。仍在运行的组件保持运行：nonebot 存活而 harness 不可用时，bridge 对触发消息回复 `[bridge] maintenance_notice` 配置的文字。
4. 交互界面显示维护横幅。人工处理后执行 `resume`：运行 `on_exit` 命令，清除标记与计数，重新执行启动顺序第 7 步起的流程。

### 6.7 进程终止

终止组件时：先从 `/proc` 收集该组件根进程的完整子孙进程树（pi 会把 MCP stdio 服务器放进独立进程组，只向组件的进程组发信号无法覆盖它们），向根进程发送 SIGTERM，等待 `term_grace`，再对之前收集到的全部 PID 中仍存活的进程发送 SIGKILL。先收集再终止，是因为父进程退出后子进程会被过继给 tini，事后已无法追溯归属。

### 6.8 代码快照

快照仓库 `/bearbot/profile/bearbotd/snapshots.git`，工作目录为 `/bearbot`，`info/exclude` 先排除全部文件，再放行以下路径：

```
nonebot/plugins/**
profile/agent/extensions/**   profile/agent/skills/**   profile/agent/prompts/**
profile/agent/settings.json   profile/agent/mcp.json    profile/agent/models.json
profile/agent/AGENTS.md       profile/agent/APPEND_SYSTEM.md   profile/agent/SYSTEM.md
workspaces/*/AGENTS.md        workspaces/*/.pi/**
```

同时排除 `__pycache__/`、`node_modules/`、`*.db`。`auth.json`、`mcp-auth.json`、会话文件不进入快照。

快照仓库归 root 所有，agent 可以读取，用 `git --git-dir=/bearbot/profile/bearbotd/snapshots.git --work-tree=/bearbot diff last-good` 查看改动，用 `git ... show last-good:<路径>` 取回旧内容。需要整体恢复时调用 `bearbot_snapshot_restore` 工具，由 daemon 执行 `git restore`。

### 6.9 命令行界面

- `docker run -it` 启动后用 `docker attach bearbot` 进入，`Ctrl-P Ctrl-Q` 离开。终端处于 raw 模式，`Ctrl-C` 只清空当前输入行。
- 交互界面使用 reeflective/console，日志行打印在输入提示上方，不打乱当前输入。stdin 为非终端时改为逐行读取命令。
- 同一套 cobra 命令也可以通过 `docker exec bearbot bearbotd ctl <命令>` 执行：`ctl` 把参数经 `admin.sock` 发给 daemon，输出以流的方式返回。

| 命令 | 作用 |
|---|---|
| `status` | 组件状态、PID、运行时长、重启次数、当前故障、是否维护模式、各 pi 会话（工作区、会话 id、空闲时长） |
| `logs <daemon\|harness\|nonebot\|pi:<会话id>> [-n 行数] [-f]` | 查看日志 |
| `restart <harness\|nonebot\|all>`、`stop <组件>`、`start <组件>` | 人工控制组件 |
| `admin token [--ttl 10m]` | 经 bridge 生成一次性管理员令牌并打印 |
| `admin list`、`admin revoke <身份>` | 管理员管理 |
| `ws list` | 工作区与绑定的场景 |
| `chat <工作区>` | 以 `console` 身份（管理员权限）在终端中与该工作区的 agent 对话，`/exit` 退出 |
| `incident show [id]`、`incident list` | 故障记录 |
| `repair [--with main\|recovery] [说明]` | 人工发起修复 |
| `snapshot list`、`snapshot diff <ref>` | 快照查看 |
| `maintenance enter [原因]`、`resume` | 进入、退出维护模式 |

### 6.10 agent API（agent.sock）

HTTP/JSON，监听 Unix socket，用 `SO_PEERCRED` 取得调用方 PID 写入日志。

| 接口 | 对应工具 | 说明 |
|---|---|---|
| `GET /v1/status` | `bearbot_status` | 组件状态、当前故障 |
| `GET /v1/logs/{组件}?n=` | `bearbot_logs` | 日志末尾 |
| `POST /v1/nonebot/check` | `bearbot_nonebot_check` | 以 `bot.py --check` 试加载插件，返回加载成功的插件与失败的回溯信息，不启动驱动 |
| `POST /v1/nonebot/restart` | `bearbot_nonebot_restart` | 重启 nonebot，等待握手，返回插件加载报告；握手失败按 `nonebot_not_ready` 建立故障 |
| `POST /v1/pi/check {cwd}` | `bearbot_pi_check` | 用 shim 相同的参数在 cwd 拉起 `pi --mode rpc --no-session`，发送 `get_state` 与 `get_commands`，收集 3 秒 stderr 后关闭，返回结果 |
| `POST /v1/session/reload {sessionId, scope}` | `bearbot_reload` | 经 `_bearbot/session/reload` 让 bridge 在当前回合结束后重新加载会话；`scope` 为 `session`、`workspace`、`all` |
| `POST /v1/im/send`、`/v1/im/actions`、`/v1/im/call` | `im_send`、`im_actions`、`im_call` | 转发给 bridge；nonebot 不可用时返回 503 |
| `POST /v1/snapshot/restore {ref, paths}` | `bearbot_snapshot_restore` | 从快照恢复文件 |
| `POST /v1/repair/{故障id}/report` | `bearbot_repair_report` | 修复报告 |
| `POST /v1/internal/shim/*` | — | shim 上报 |

需要管理员权限的工具（`bearbot_nonebot_restart`、`bearbot_snapshot_restore`）在核心扩展中按运行角色检查（第 7.2 节）。

## 7. pi harness

### 7.1 进程与环境

daemon 拉起 pi-acp 时的环境变量：

```
HOME=/home/bearbot
PATH=/bearbot/profile/bin:/usr/local/bin:/usr/bin:/bin
PI_CODING_AGENT_DIR=/bearbot/profile/agent
PI_ACP_PI_COMMAND=/usr/local/bin/bearbot-pi
PI_SKIP_VERSION_CHECK=1
BEARBOT_AGENT_SOCKET=/run/bearbot/agent.sock
BEARBOT_WORKSPACES=/bearbot/workspaces
<[harness.env] 中配置的模型服务变量，如 ANTHROPIC_API_KEY>
```

模型服务凭据可以放在 `[harness.env]`，也可以在 profile 中用 pi 登录：`docker exec -it -u bearbot bearbot pi`，在 pi 中执行 `/login`，凭据写入 `/bearbot/profile/agent/auth.json`，全部 harness 共用。

### 7.2 核心扩展（`/opt/bearbot/pi-extension/core`）

#### 项目信任

处理 `project_trust`：`realpath(cwd)` 位于 `/bearbot/workspaces/` 下且不在 `.archive/` 中时返回 `{ trusted: "yes", remember: false }`，其余返回 `no`。

#### 信封解析与发送者感知

处理 `input`（`source === "rpc"`）：

1. 文本以 `@bearbot/v1\n` 开头时解析其后的 JSON（格式见第 8 节）。解析失败返回 `handled`，并用 `pi.sendMessage` 写入一条错误说明，不启动运行。
2. 不以该前缀开头的普通文本同样返回 `handled` 并写入错误说明。只有 bridge 和 daemon 会向 pi-acp 发送提示，它们总是使用信封，出现其他输入说明调用方有缺陷。
3. 解析成功后把会话状态更新为：场景、工作区、本轮角色、本轮允许的工具，并返回 `transform`，把信封渲染成模型阅读的文本：

```
<bearbot-turn kind="chat" workspace="ws1" scene="QQ 群「测试群」(QQClient/10000/group/123)" role="member">
<observed>
[14:01] Bob (QQClient:456): 今天天气怎么样
</observed>
<message id="m-889" from="Alice (QQClient:123)" sender-role="admin" member-role="owner" time="2026-10-26T14:02:11+08:00" mentions-bot="true">
<reply-to from="Bob (QQClient:456)">今天天气怎么样</reply-to>
帮 Bob 查一下
<attachment type="file" path="inbox/2026-10-26/a.pdf" name="a.pdf" size="20480"/>
</message>
<extra-context>……插件追加的内容……</extra-context>
</bearbot-turn>
```

用户文本中的 `<` 与 `>` 转义为 `&lt;`、`&gt;`。权限判断只使用解析出的结构化数据，模型看到的文本不参与判断。

#### 权限控制

- 角色由 bridge 计算：`system` > `admin` > `member`；一轮包含多条消息时取其中最低的角色。
- `before_agent_start`：按本轮 `allow_tools` 调用 `pi.setActiveTools()`，系统提示中注明当前角色。
- `tool_call`：工具名不匹配 `allow_tools` 中任何模式时阻止调用，原因写明“当前发送者无权使用该工具”。codemode 脚本发起的嵌套调用同样受检查。
- `member` 角色时，即使 `write`、`edit` 被允许，路径位于 `.pi/` 之下或位于 `/bearbot/workspaces/<当前工作区>` 之外时也会阻止。
- 默认访客工具：`read`、`grep`、`find`、`ls`、`im_send`、`im_actions`、`im_call`。管理员可以按工作区修改（第 9.5 节）。

#### 记忆与系统提示

- `before_agent_start` 向 `systemPromptOptions.sections.bearbot` 写入 bearbot 说明（内容来自只读的 `prompt/bearbot.md`）：
  - 输入格式、角色含义、在群聊中如何区分发送者；
  - 工作区根目录的 `AGENTS.md` 是本工作区的长期记忆，会话开始和上下文压缩后会重新读取，应把需要长期记住的事实、偏好、约定写进去，并保持精简；
  - `/bearbot/profile/agent/AGENTS.md` 是跨工作区的全局记忆；
  - 资料和文档存放在工作区的 `docs/`，在 `AGENTS.md` 中记录索引；
  - 最终回复文本会自动发回当前场景；只在需要发送附件、发送到其他场景、在长任务中途汇报时使用 `im_send`；不需要回复时最终文本只写 `NO_REPLY`；
  - 自我扩展的位置与规则（第 7.4 节），以及修改后必须先调用检查工具再重载。
- `session_compact`：重新读取工作区与全局的 `AGENTS.md`，标记待更新；若 `ctx.isIdle()` 为假（压缩发生在运行中），立即用 `pi.sendMessage({ customType: "bearbot-memory", content }, { deliverAs: "steer" })` 把最新内容送入当前运行。
- `before_agent_start` 发现待更新标记时，替换 `systemPromptOptions.contextFiles` 中这两个文件的内容，pi 会把系统提示的变化追加到会话记录中，然后清除标记。

#### 注册的工具

| 工具 | 默认角色 | 说明 |
|---|---|---|
| `im_send` | member | 发送文本、图片路径、文件路径、提及到当前场景；`scene` 参数可指定同一工作区绑定的其他场景，`system`/`admin` 角色可指定任意场景 |
| `im_actions` | member | 列出 nonebot 插件为当前工作区注册的动作（第 9.7 节） |
| `im_call` | member | 调用动作；动作声明了 `admin_only` 时由 bridge 再次检查角色 |
| `bearbot_status`、`bearbot_logs` | admin | 查看组件状态与日志 |
| `bearbot_nonebot_check`、`bearbot_nonebot_restart` | admin | 检查与重启 nonebot |
| `bearbot_pi_check`、`bearbot_reload` | admin | 检查与重新加载 pi 扩展 |
| `bearbot_snapshot_restore` | admin | 从快照恢复 |
| `bearbot_repair_report` | system | 仅在 `kind: "system"` 且带 `incident` 的运行中启用 |

`bearbot_repair_report` 的参数：

```ts
{
  incident: string,
  status: "fixed" | "no_change" | "cannot_fix",
  summary: string,          // 原因与处理过程
  changed_files: string[],
  memory_note?: string      // 写入 _system/AGENTS.md 的经验
}
```

### 7.3 recovery 扩展（`/opt/bearbot/pi-extension/recovery`）

只注册第 6.6 节列出的工具，并通过 `before_agent_start` 加入 recovery 规则：只修复导致故障的文件；优先用快照恢复最近的改动；不修改 `/bearbot/profile/agent/auth.json`；完成后必须调用 `bearbot_repair_report`。recovery 运行没有信封解析，daemon 直接发送纯文本的故障报告。

### 7.4 自我扩展规则

| 目标 | 写入位置 | 生效方式 |
|---|---|---|
| 只影响一个工作区的 pi 能力 | `<工作区>/.pi/extensions/`、`.pi/skills/`、`.pi/mcp.json`、`.pi/settings.json` | `bearbot_pi_check` 通过后 `bearbot_reload(scope: "workspace")` |
| 影响全部工作区的 pi 能力 | `/bearbot/profile/agent/extensions/` 等 | 必须对 `_system` 执行 `bearbot_pi_check`，通过后 `bearbot_reload(scope: "all")` |
| 某个工作区专属的 nonebot 插件 | `/bearbot/nonebot/plugins/ws_<工作区>_<名称>/`，`__plugin_meta__.extra` 中写 `{"bearbot_workspace": "<工作区>"}` | `bearbot_nonebot_check` 通过后 `bearbot_nonebot_restart` |
| 跨工作区的 nonebot 插件 | `/bearbot/nonebot/plugins/<名称>/` | 同上 |
| 插件需要的 Python 依赖 | `uv pip install --target /bearbot/nonebot/site-packages <包>` | 重启 nonebot |
| pi 扩展需要的 npm 依赖 | 扩展目录旁的 `package.json`，在该目录执行 `npm install` | 重新加载 |

`bearbot_reload` 的实现：bridge 在相关会话当前回合结束后执行 `session/load`。pi-acp 处理 `session/load` 时会关闭旧 pi 进程、按会话文件拉起新进程，新进程重新加载扩展、技能、MCP 配置和 `AGENTS.md`，会话历史保留。新进程拉起失败时 `session/load` 返回错误，bridge 向 daemon 报告，按 `pi_session_exit` 建立故障。

镜像自带三个技能，放在 `/opt/bearbot/skills`：

- `bearbot-nonebot-plugin`：第 9.7 节的扩展接口、插件模板、命名规则、检查与重启流程、定时任务写法（apscheduler + `bearbot.prompt`）。
- `bearbot-pi-extension`：pi 扩展文档位置（镜像内 pi 包的 `docs/`、`examples/`）、工作区与全局扩展的区别、检查与重载流程。
- `bearbot-repair`：修复流程、日志位置、快照用法、报告要求。

## 8. 信封格式

提示词为单个文本块，内容为 `@bearbot/v1\n` 加 JSON；消息中的图片额外作为 ACP `image` 块附在后面（pi-acp 支持图片）。

```json
{
  "v": 1,
  "kind": "chat",
  "workspace": "ws1",
  "scene": {
    "key": "QQClient/10000/group/123",
    "adapter": "OneBot V11",
    "scope": "QQClient",
    "self_id": "10000",
    "type": "group",
    "id": "123",
    "parent_id": null,
    "name": "测试群"
  },
  "role": "member",
  "allow_tools": ["read", "grep", "find", "ls", "im_send", "im_actions", "im_call"],
  "messages": [
    {
      "id": "m-889",
      "time": "2026-10-26T14:02:11+08:00",
      "sender": {
        "id": "QQClient:123",
        "name": "Alice",
        "role": "admin",
        "member_role": "owner"
      },
      "text": "帮 Bob 查一下",
      "mentions_bot": true,
      "reply_to": { "id": "m-880", "sender": "QQClient:456", "name": "Bob", "text": "今天天气怎么样" },
      "attachments": [
        { "type": "image", "block_index": 1 },
        { "type": "file", "path": "inbox/2026-10-26/a.pdf", "name": "a.pdf", "size": 20480 }
      ]
    }
  ],
  "observed": [
    { "id": "m-880", "time": "…", "sender": { "id": "QQClient:456", "name": "Bob", "role": "member" }, "text": "今天天气怎么样" }
  ],
  "extra_context": [],
  "incident": null
}
```

| 字段 | 说明 |
|---|---|
| `kind` | `chat`：来自聊天；`plugin`：nonebot 插件通过 `bearbot.prompt` 发起；`console`：交互界面 `chat`；`system`：daemon 发起的修复或通知 |
| `scene.key` | `scope/self_id/scene_type/scene_id`，频道类场景为 `scope/self_id/channel/<parent_id>:<id>` |
| `sender.id` | `scope:user_id`，同一平台上同一用户在不同机器人账号下身份相同 |
| `role` | 本轮角色，取 `messages` 中最低的发送者角色；`plugin` 的角色由调用方指定，`console`、`system` 固定 |
| `allow_tools` | 工具名模式列表，支持 `*` 通配，`["*"]` 表示不限制 |
| `observed` | 上一轮之后场景中出现、未触发 agent 的消息，最多 `observe_limit` 条 |
| `incident` | `kind: "system"` 时的故障报告 |

## 9. nonebot bridge（`nonebot_plugin_bearbot`）

### 9.1 启动入口 `bot.py`

```
1. 把 /bearbot/nonebot/site-packages 追加到 sys.path 末尾（镜像内的包优先，agent 安装的包不能覆盖它们）
2. nonebot.init(_env_file=())，配置只来自 daemon 传入的环境变量
3. 按 BEARBOT_NONEBOT_ADAPTERS 注册适配器
4. nonebot.load_plugin("nonebot_plugin_bearbot")
5. nonebot.load_plugins(*BEARBOT_NONEBOT_EXTRA_PLUGINS)（配置中声明的 PyPI 插件）
6. 安装临时 loguru sink，对 /bearbot/nonebot/plugins 下每个模块调用 nonebot.load_plugin，记录失败的模块名与回溯
7. 把加载报告交给 bridge，在 driver.on_startup 中随 hello 发送
8. --check 模式：完成 1～6 后以 JSON 输出报告并退出，不运行驱动
9. nonebot.run()
```

### 9.2 ACP 客户端

- `driver.on_startup` 中 `asyncio.open_unix_connection("/run/bearbot/acp.sock")`，`acp.connect_to_agent(client, writer, reader)`，`initialize`，发送 `_bearbot/bridge/hello`，然后对数据库中每个会话调用 `_bearbot/session/attach`，发送暂存的回复。
- 连接断开时 bridge 记录错误并调用 `nonebot` 的关闭流程使进程退出，由 daemon 判断并处理。daemon 是连接的另一端，连接断开说明 daemon 自身在退出或重启。
- `Client.session_update` 只在该会话有进行中的 `prompt` 时收集更新，其余（启动信息、`session/load` 的历史回放）丢弃。
- `Client.ext_method` 处理第 6.3 节中 daemon → bridge 的方法。

### 9.3 数据模型（nonebot-plugin-orm，SQLite）

| 表 | 字段 |
|---|---|
| `admin` | `identity` 主键，`display_name`，`notify_target`（alconna `Target.dump()`，领取令牌时所在场景），`added_by`，`added_at` |
| `admin_token` | `token_hash` 主键（SHA-256），`created_at`，`expires_at`，`used_by`，`used_at` |
| `workspace` | `id` 主键，`description`，`guest_tools`（JSON），`created_by`，`created_at`，`archived_at` |
| `binding` | `scene_key` 主键，`workspace_id`，`target`（`Target.dump()`），`trigger`（`mention`/`prefix`/`all`），`prefix`，`session_mode`（`scene`/`sender`），`reply_mode`（`final`/`segments`），`created_by`，`created_at` |
| `agent_session` | （`scene_key`, `sender_key`, `workspace_id`）主键，`acp_session_id`，`needs_load`，`created_at`，`last_used_at` |

`session_mode = scene` 时整个场景共用一个会话，`sender_key` 为空字符串；`session_mode = sender` 时场景中每个发送者各有一个会话。私聊场景两者相同。

### 9.4 管理员令牌

1. 交互界面执行 `admin token`，daemon 向 bridge 发送 `_bearbot/admin/createToken {ttl}`。
2. bridge 生成 `bearbot-` 加 16 个 base32 字符（80 位随机数），保存哈希与过期时间，返回明文令牌，daemon 打印令牌与过期时间。bridge 未连接时命令报错。
3. 在 bridge 可见的任意场景中发送这段令牌（消息纯文本去除首尾空白后与令牌完全相同）。bridge 用独立的高优先级消息匹配器比对未过期、未使用的哈希，命中后把发送者加入 `admin`，`notify_target` 记为该场景，令牌标为已用，回复确认。
4. 之后管理员可以用 `/bb admin add <@用户|身份>` 添加其他管理员。

### 9.5 管理命令

命令用 nonebot-plugin-alconna 定义，前缀 `/bb`，仅管理员可用；非管理员发送时不回应。

| 命令 | 作用 |
|---|---|
| `/bb ws list` | 工作区列表与绑定数量 |
| `/bb ws create <id> [描述]` | 创建工作区目录、模板 `AGENTS.md`、`.pi/`、`docs/`、`inbox/`，写入数据库 |
| `/bb ws info [id]` | 工作区信息、绑定的场景、会话 |
| `/bb ws desc <id> <描述>` | 修改描述 |
| `/bb ws guest-tools <id> <模式列表>` | 修改访客可用工具 |
| `/bb ws archive <id>` | 解除全部绑定，目录移动到 `.archive/` |
| `/bb attach <id> [--trigger mention\|prefix\|all] [--prefix <文本>] [--session scene\|sender] [--reply final\|segments]` | 把当前场景绑定到工作区；私聊默认 `all`，群聊默认 `mention` |
| `/bb detach` | 解除当前场景的绑定 |
| `/bb here` | 当前场景的 key、绑定、会话 |
| `/bb new` | 为当前场景（或当前发送者）开启新会话 |
| `/bb stop` | 向当前会话发送 `session/cancel` |
| `/bb compact [说明]` | 向当前会话发送 `/compact [说明]` |
| `/bb reload` | 当前回合结束后对当前会话执行 `session/load` |
| `/bb admin list\|add\|remove` | 管理员管理 |
| `/bb status` | 经 `_bearbot/status` 查询 daemon |
| `/bb restart nonebot\|harness` | 经 `_bearbot/restart` 请求 daemon 重启 |

### 9.6 消息处理流程

```
适配器消息事件
  │
  ├─ 优先级 1：令牌匹配器（命中则处理并 block）
  ├─ 优先级 2：/bb 命令（block）
  └─ 优先级 50：bridge 主匹配器（block=False，不影响其他插件的普通消息匹配器）
        1. 用 uninfo 取得场景与发送者，查找绑定；未绑定则结束
        2. 把消息转换为信封中的消息结构：
             文本原样；提及写成 @名称(身份)；
             图片下载后作为 ACP image 块；
             文件下载到 <工作区>/inbox/<日期>/，记录相对路径；
             引用消息取被引用的内容
        3. 构造 WorkspaceMessageEvent，await handle_event(bot, event)
        4. 事件被 stop_forward() 时结束
        5. 不满足触发条件（mention / prefix / all）时放入该场景的旁观缓冲区，结束
        6. 放入该会话的发送队列
```

每个会话一个异步任务处理发送队列：

1. 取出队列中全部待发送消息（回合进行中到达的消息在下一轮合并发送），连同旁观缓冲区、插件追加的 `extra_context` 组成信封。
2. 会话不存在时 `session/new`（cwd 为工作区目录）；`needs_load` 为真时先 `session/load`。
3. `prompt`，收集 `agent_message_chunk`；`reply_mode = segments` 时遇到 `tool_call` 更新就把已收集的文字作为一段发送。
4. 回合结束，若文本为空或为 `NO_REPLY` 则不发送；否则构造 `AgentReplyEvent`，`await handle_event`，未被 `stop_send()` 时用 `UniMessage` 发送，超过 `max_message_length` 时分段。
5. 请求返回错误时，向场景回复错误摘要（管理员可见完整错误，访客只看到简短提示），错误写入日志。不自动重试。

harness 不可用（收到 `_bearbot/harness/state {state: "down"}`）时，触发消息不进入队列，回复 `maintenance_notice`。

### 9.7 扩展接口

bridge 以 Python 包的形式提供以下接口，供 agent 编写的插件使用（`require("nonebot_plugin_bearbot")` 后导入）。

#### 事件

```python
class WorkspaceMessageEvent(Event):
    workspace_id: str
    scene: SceneInfo                  # key、类型、id、名称、Target
    sender: SenderInfo                # 身份、名称、角色、群内角色
    message: UniMessage
    origin: Event                     # 原始适配器事件
    extra_context: list[str]          # 追加到信封的内容

    def get_type(self) -> str: return "bearbot_workspace_message"
    def stop_forward(self) -> None    # 本条消息不再交给 agent
    async def reply(self, message: str | UniMessage) -> None


class AgentReplyEvent(Event):
    workspace_id: str
    scene: SceneInfo
    session_id: str
    message: UniMessage               # 可直接修改
    turn: TurnInfo                    # 本轮的信封、停止原因、用到的工具名

    def get_type(self) -> str: return "bearbot_agent_reply"
    def stop_send(self) -> None
```

`get_type()` 使用自定义类型，`on_message` 匹配器不会再次收到这些事件。

```python
def on_workspace_message(workspace: str | None = None, *, rule=None, priority=10, block=False, **kwargs) -> type[Matcher]
def on_agent_reply(workspace: str | None = None, *, rule=None, priority=10, block=False, **kwargs) -> type[Matcher]
```

`workspace=None` 时匹配全部工作区。

#### 函数

```python
async def prompt(workspace: str, scene_key: str, text: str, *, role: Literal["admin", "member"] = "member",
                 send_reply: bool = True) -> str     # 以 kind="plugin" 发起一轮，返回最终回复文本
async def send(scene_key: str, message: str | UniMessage) -> None
def workspaces() -> list[WorkspaceInfo]
def bindings(workspace: str | None = None) -> list[BindingInfo]
def is_admin(identity: str) -> bool
```

#### 动作

```python
@agent_action("weather.query", workspace="ws1", description="查询城市天气", admin_only=False)
async def weather(ctx: ActionContext, city: str) -> dict: ...
```

- 参数的 JSON Schema 由函数签名通过 pydantic 生成，`im_actions` 返回名称、描述、参数结构。
- `ActionContext` 包含调用方工作区、场景、角色、对应的 `Bot`。
- `workspace=None` 的动作对全部工作区可用。
- 内置动作 `bot.call_api`（`admin_only=True`）：用当前场景的 `Bot` 调用任意平台接口。

### 9.8 插件异常记录

bridge 注册 `run_postprocessor`，匹配器抛出异常时把插件名、所属工作区（取自 `__plugin_meta__.extra.bearbot_workspace`）、事件摘要、回溯追加到 `/bearbot/nonebot/errors.jsonl`。agent 用 `bearbot_logs nonebot-errors` 读取。这类异常不建立故障。

### 9.9 Python 依赖

镜像内 `/opt/bearbot/venv` 安装：`nonebot2[fastapi,httpx,websockets]`、`nonebot-plugin-alconna`、`nonebot-plugin-uninfo`、`nonebot-plugin-localstore`、`nonebot-plugin-orm[default]`、`nonebot-plugin-apscheduler`、`agent-client-protocol`、`nonebot-adapter-onebot`、`nonebot-adapter-satori`、`nonebot-adapter-qq`、`nonebot-adapter-telegram`。其他适配器或 PyPI 插件写在 `[nonebot] packages` 中，由 daemon 安装到 `site-packages/`。

## 10. 配置文件 `bearbot.toml`

```toml
[daemon]
log_level = "info"
shutdown_grace = "60s"
term_grace = "10s"

[harness]
idle_evict_after = "30m"
probe_timeout = "60s"
[harness.env]
ANTHROPIC_API_KEY = "${ANTHROPIC_API_KEY}"      # ${} 从容器环境变量取值

[nonebot]
driver = "~fastapi+~httpx+~websockets"
host = "0.0.0.0"
port = 8080
adapters = ["nonebot.adapters.onebot.v11"]
packages = []                                   # 额外安装的 PyPI 包
plugins = []                                    # 额外加载的已安装插件模块
ready_timeout = "90s"
[nonebot.env]
ONEBOT_ACCESS_TOKEN = "${ONEBOT_ACCESS_TOKEN}"
COMMAND_START = '["/"]'

[bridge]
default_guest_tools = ["read", "grep", "find", "ls", "im_send", "im_actions", "im_call"]
observe_limit = 30
max_message_length = 2000
maintenance_notice = "bearbot 正在维护，暂时无法回复。"
token_ttl = "10m"
notify_admins = true                            # 故障开始、结束、进入维护模式时私聊通知管理员

[supervisor]
stable_after = "120s"
repair_timeout = "30m"
main_attempts = 2
recovery_attempts = 1

[recovery]
provider = "anthropic"
model = "claude-opus-5-5"
api_key = "${BEARBOT_RECOVERY_API_KEY}"          # 未设置时使用 /bearbot/config/recovery/auth.json
timeout = "30m"

[maintenance]
on_enter = ["/bearbot/config/hooks/notify.sh"]
on_exit = []
hook_timeout = "30s"
```

所有时长用 Go `time.ParseDuration` 解析；`${VAR}` 引用的环境变量不存在时 daemon 启动失败。

## 11. Dockerfile

```dockerfile
# syntax=docker/dockerfile:1.7
FROM golang:1.27-bookworm AS daemon
WORKDIR /src
COPY daemon/go.mod daemon/go.sum ./
RUN go mod download
COPY daemon/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bearbotd ./cmd/bearbotd

FROM ghcr.io/astral-sh/uv:0.13 AS uv

FROM node:24-bookworm-slim
ARG PI_VERSION=1.4.0
ARG PI_ACP_VERSION=0.0.34
ARG PYTHON_VERSION=3.12
ARG BEARBOT_UID=1000
ARG BEARBOT_GID=1000

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git jq ripgrep procps tini build-essential \
 && rm -rf /var/lib/apt/lists/*

COPY --from=uv /uv /uvx /usr/local/bin/
ENV UV_PYTHON_INSTALL_DIR=/opt/bearbot/python \
    UV_LINK_MODE=copy

RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} pi-acp@${PI_ACP_VERSION}

COPY nonebot/ /opt/bearbot/nonebot/
RUN uv python install ${PYTHON_VERSION} \
 && uv venv --python ${PYTHON_VERSION} /opt/bearbot/venv \
 && uv pip install --python /opt/bearbot/venv/bin/python /opt/bearbot/nonebot

COPY pi-extension/ /opt/bearbot/pi-extension/
RUN cd /opt/bearbot/pi-extension/core && npm ci --omit=dev \
 && cd /opt/bearbot/pi-extension/recovery && npm ci --omit=dev
COPY skills/ /opt/bearbot/skills/

COPY --from=daemon /out/bearbotd /usr/local/bin/bearbotd
COPY docker/bearbot-pi /usr/local/bin/bearbot-pi

RUN groupadd -g ${BEARBOT_GID} bearbot \
 && useradd -m -u ${BEARBOT_UID} -g bearbot -s /bin/bash bearbot \
 && ln -s /bearbot/profile /home/bearbot/.pi \
 && mkdir -p /bearbot/profile /bearbot/workspaces /bearbot/nonebot /bearbot/config \
 && chown bearbot:bearbot /bearbot/profile /bearbot/workspaces /bearbot/nonebot \
 && chmod 0700 /bearbot/config \
 && chmod 0755 /usr/local/bin/bearbot-pi \
 && chown -R root:root /opt/bearbot && chmod -R go-w /opt/bearbot

ENV PI_CODING_AGENT_DIR=/bearbot/profile/agent \
    PI_SKIP_VERSION_CHECK=1

VOLUME ["/bearbot/profile", "/bearbot/workspaces", "/bearbot/nonebot", "/bearbot/config"]
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/bearbotd"]
CMD ["run", "--config", "/bearbot/config/bearbot.toml"]
```

`docker/bearbot-pi`：

```sh
#!/bin/sh
exec /usr/local/bin/bearbotd pi-shim -- "$@"
```

运行方式：

```sh
docker run -dit --name bearbot --stop-timeout 90 \
  -v bearbot-profile:/bearbot/profile \
  -v /srv/bearbot/workspaces:/bearbot/workspaces \
  -v /srv/bearbot/nonebot:/bearbot/nonebot \
  -v /srv/bearbot/config:/bearbot/config \
  -e ANTHROPIC_API_KEY -e BEARBOT_RECOVERY_API_KEY -e ONEBOT_ACCESS_TOKEN \
  -p 8080:8080 bearbot
docker attach bearbot
```

## 12. 仓库结构

```
bearbot/
├── DESIGN.md
├── Dockerfile
├── docker/bearbot-pi
├── config/bearbot.example.toml
├── daemon/                         Go 模块（第 6.1 节）
├── nonebot/
│   ├── pyproject.toml
│   ├── bot.py
│   └── nonebot_plugin_bearbot/
│       ├── __init__.py             插件元数据、对外接口导出
│       ├── config.py
│       ├── models.py  migrations/  nonebot-plugin-orm 模型与迁移
│       ├── acp_client.py           ACP 连接、扩展方法、会话队列
│       ├── envelope.py             信封构造
│       ├── convert.py              UniMessage 与信封消息的相互转换
│       ├── events.py               WorkspaceMessageEvent、AgentReplyEvent、匹配器工厂
│       ├── actions.py              agent_action 注册与调用
│       ├── commands.py             /bb 命令、令牌匹配器
│       └── api.py                  prompt / send / workspaces / bindings / is_admin
├── pi-extension/
│   ├── core/{package.json,index.ts,envelope.ts,policy.ts,memory.ts,tools.ts,prompt/bearbot.md}
│   └── recovery/{package.json,index.ts,prompt/recovery.md}
└── skills/
    ├── bearbot-nonebot-plugin/SKILL.md
    ├── bearbot-pi-extension/SKILL.md
    └── bearbot-repair/SKILL.md
```

## 13. 安全说明

| 受保护 | 手段 |
|---|---|
| bridge、核心扩展、recovery 扩展、daemon 的代码 | root 所有的只读文件 |
| 配置文件与 recovery 凭据 | `/bearbot/config` 权限 0700；recovery 凭据只在 recovery 修复期间出现在 tmpfs |
| 管理员令牌生成、维护模式控制 | 仅 root 可用的 `admin.sock` |
| 访客发起的运行中的高权限工具 | 核心扩展按信封中的角色限制工具，codemode 嵌套调用同样受限 |
| 被访客文本伪造的身份 | 身份只取自信封 JSON；用户文本只出现在字符串字段中，渲染时转义 |

| 不受保护 | 说明 |
|---|---|
| 工作区之间的隔离 | 全部 pi 进程以同一用户运行，可以读写所有工作区；`im_send` 的场景检查只防止误发 |
| 授予访客 `bash` 之后的权限 | `bash` 可以访问 `agent.sock`、`acp.sock` 和全部 agent 可写目录，等同于管理员权限；`/bb ws guest-tools` 设置包含 `bash` 的列表时 bridge 要求管理员再次确认 |
| profile 中的模型服务凭据 | agent 可以读取 `auth.json`，这是 pi 运行的前提 |
| 提示注入 | 聊天内容、网页、MCP 返回值都可能影响模型行为；限制访客工具是主要的应对方式 |

## 14. 验证方案

全部验证使用真实组件，不使用 mock：

- 路由：在测试中拉起真实的 pi-acp（`PI_ACP_PI_COMMAND` 指向测试构建的 shim），用 acp-go-sdk 客户端从多个连接执行 `initialize`、`session/new`、`session/load`、断开后 `_bearbot/session/attach`；这些步骤不需要模型调用。
- shim：拉起真实 pi，验证 `get_state` 解析、stderr 日志、空闲回收、SIGTERM 转发、异常退出上报。异常退出用一个在工厂函数中调用 `process.exit(1)` 的真实扩展触发。
- 故障处理：在容器中放入会导致 nonebot 导入失败的插件、会导致 pi 启动失败的全局扩展、无效的 recovery 凭据，使用真实模型服务走完主修复、recovery 修复、维护模式三条路径，检查 `incidents/` 记录与维护脚本的执行。
- bridge：用 OneBot V11 适配器连接真实的 QQ 协议端（或 Satori 连接真实平台），验证令牌领取、绑定、群聊触发、旁观缓冲、图片与文件、`AgentReplyEvent`、`agent_action`。
- 核心扩展：用真实模型分别以 `member`、`admin` 角色的信封运行，检查 `tool_call` 拦截记录与 `AGENTS.md` 在 `/compact` 之后的重新加载。
