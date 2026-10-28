# bearbot

bearbot 是一个能自我演进的 IM 机器人。它把 [pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) 接入 Telegram 和 QQ 官方机器人，让 pi 在群聊和私聊中工作。管理员在聊天中提出需求，agent 编写 pi 扩展、技能、MCP 配置给自己增加功能；改动经过检查后生效，改坏了由独立的 recovery agent 诊断修复，修复不了时进入维护状态，管理员可以从聊天或命令行回滚。

一个容器运行一个 agent，使用一个工作区。需要多个互相独立的 bot 时，用不同的容器名与挂载目录运行多个容器。

完整的设计与实现说明见 [DESIGN.md](DESIGN.md)。`DESIGN_MULTI_WORKSPACE.md` 与 `DESIGN_STALE.md` 是已放弃的方案，不描述现在的代码。

## 组成

| 组件 | 目录 | 作用 |
|---|---|---|
| `bearbotd` | `daemon/` | Go 编写的容器入口。启动并监视其余进程；执行检查、应用与回滚；处理故障并调度 recovery；维护快照；提供 `docker exec` 使用的命令行 |
| gateway | `gateway/` | 运行 chat-sdk 的 Node 进程。连接各 IM 平台与控制台；处理管理员令牌与 `/bb` 命令；保存管理员、绑定、消息记录（SQLite） |
| `bearbot-im` | `extensions/bearbot-im/` | 加载到 agent 进程中的 pi 扩展。连接 gateway，把消息交给 agent，把回复发回 IM；检查成员发起的回合中的工具调用（classifier）；向其他扩展提供订阅与中间件 API |
| bearbot SDK | `sdk/index.ts` | 其他扩展取得 `bearbot-im` API 用的辅助模块与类型定义 |
| recovery 扩展 | `extensions/bearbot-recovery/` | recovery agent 使用的工具：读取故障信息、提交修复报告 |
| 自带技能 | `skills/` | `bearbot-extension-dev`、`bearbot-middleware`、`bearbot-recovery` |
| recovery profile 模板 | `recovery-profile/settings.json` | recovery 使用的 pi 设置 |
| 示例配置 | `config/bearbot.example.toml` | 全部配置项与说明 |

## 构建镜像

```sh
docker build -t bearbot .
```

可以用 `--build-arg` 修改 `PI_VERSION`、`TYPESCRIPT_VERSION`、`PYTHON_VERSION`、`BEARBOT_UID`、`BEARBOT_GID`、`BEARBOT_GW_UID`，默认值写在 `Dockerfile` 中。

## 运行

### 准备挂载目录

容器有三个挂载点，启动前必须都已经存在：

| 挂载点 | 内容 |
|---|---|
| `/bearbot/profile` | pi profile（凭据、会话、设置、模型定义）、Python 虚拟环境、agent 安装的可执行文件、bearbot 的日志与快照 |
| `/bearbot/workspace` | 工作区：记忆 `AGENTS.md`、classifier 补充规则 `CLASSIFIER.md`、agent 的扩展与配置 `.pi/`、资料、收到的文件 |
| `/bearbot/config` | `bearbot.toml`、recovery profile 的覆盖文件、维护脚本 |

```sh
mkdir -p /srv/bearbot/profile /srv/bearbot/workspace /srv/bearbot/config
cp config/bearbot.example.toml /srv/bearbot/config/bearbot.toml
```

然后按需要修改 `bearbot.toml`。配置中的 `${变量名}` 从容器的环境变量取值，变量不存在时 daemon 启动失败，所以用到的变量都要在 `docker run` 中传入。

### 模型服务

- agent 的凭据：在 `[agent.env]` 中给出 API key，或者在容器运行后执行 `docker exec -it bearbot bearbotd login`，在交互式 pi 中执行 `/login`，凭据写入 `/bearbot/profile/agent/auth.json`。
- agent 使用的模型：写在工作区的 `.pi/settings.json`（`defaultProvider`、`defaultModel`）中。自定义的模型服务（例如 OpenAI 兼容接口）写在 `/bearbot/profile/agent/models.json` 中，格式见 pi 的文档。
- classifier 使用的模型：`[classifier] model`，写法为 `<服务>/<模型 id>`。
- recovery 的凭据与模型：recovery 使用独立的 profile，不读取 agent 的 `auth.json`。凭据放在 `[recovery.env]` 中，或者放在 `/bearbot/config/recovery/auth.json`。`/bearbot/config/recovery/` 中的 `settings.json`、`models.json` 替换镜像中的模板，替换用的 `settings.json` 必须保留 `defaultProvider`。

### 启动容器

```sh
docker run -d --name bearbot --stop-timeout 120 --tmpfs /run/bearbot \
  -v /srv/bearbot/profile:/bearbot/profile \
  -v /srv/bearbot/workspace:/bearbot/workspace \
  -v /srv/bearbot/config:/bearbot/config \
  -e ANTHROPIC_API_KEY -e BEARBOT_RECOVERY_API_KEY -e TELEGRAM_BOT_TOKEN \
  bearbot
```

- `--tmpfs /run/bearbot` 是必需的，进程之间的 Unix socket 放在这里。
- `--stop-timeout` 要大于 `[daemon] shutdown_grace`（默认 90 秒），daemon 收到 SIGTERM 后等待 agent 的当前回合结束。
- 启用 `[gateway.http]` 或扩展需要对外提供端口时，另加 `-p` 映射。

### 成为管理员并绑定聊天

1. 生成一次性管理员令牌：

   ```sh
   docker exec bearbot bearbotd admin token
   ```

2. 在机器人能收到消息的任意聊天中发送这段令牌，私聊机器人最可靠。成功后机器人回复“已成为 bearbot 管理员”，之后系统通知发到这个聊天。
3. 在要交给 bot 的群或私聊中发送 `/bb attach`。bot 只处理已绑定频道中的消息；私聊默认处理全部消息，群默认只处理提及机器人的消息。

Telegram 群内要收到普通消息，需要在 BotFather 中用 `/setprivacy` 关闭隐私模式，或者把机器人设为群管理员。QQ 群内与 QQ 单聊中同一个人是两个身份，要分别领取令牌，或者由已有管理员用 `/bb admin add` 添加。

### 控制台

控制台用于本地调试：在终端中直接和运行中的 agent 对话，终端用户是内置管理员 `console:operator`，不需要领取令牌。`[platforms.console] enabled = true`（默认）并且 bearbotd 的标准输入是终端、管道或文件时启用。

```sh
docker run -dit --name bearbot --stop-timeout 120 --tmpfs /run/bearbot \
  -v /srv/bearbot/profile:/bearbot/profile \
  -v /srv/bearbot/workspace:/bearbot/workspace \
  -v /srv/bearbot/config:/bearbot/config \
  -e ANTHROPIC_API_KEY -e BEARBOT_RECOVERY_API_KEY \
  bearbot
docker attach bearbot        # 按回车显示提示符；Ctrl-P Ctrl-Q 断开，容器继续运行
```

只使用控制台时，其他平台可以全部设为 `enabled = false`。控制台中输入 `:help` 查看本地命令，`:file <路径> [文字]` 发送一个文件（相对路径从工作区目录开始），`::<文字>` 发送以 `:` 开头的普通消息，行尾为 `\` 时继续输入下一行；完整的按键与输入规则见 DESIGN.md 第 6.2.4 节。

## 聊天中的 `/bb` 命令

只有管理员可以使用，非管理员发送时不回应。

| 命令 | 作用 |
|---|---|
| `/bb help` | 命令列表 |
| `/bb here` | 当前频道、会话串、绑定设置与 agent 状态 |
| `/bb attach [--trigger mention\|all\|prefix] [--prefix <文本>] [--reply final\|each] [--interrupt queue\|steer]` | 绑定当前频道，或修改它的绑定设置 |
| `/bb detach` | 解除当前频道的绑定 |
| `/bb new` | 开启新会话，所有绑定的频道都改用新会话 |
| `/bb stop` | 中止正在进行的回合 |
| `/bb compact [说明]` | 压缩当前会话 |
| `/bb restart` | 重启 agent 进程（等待当前回合结束） |
| `/bb status` | daemon、agent、平台与绑定的状态 |
| `/bb repair [说明]`、`/bb resume` | 维护状态下重新运行 recovery 修复，或者不修复直接恢复运行 |
| `/bb rollback [快照]` | 回滚到指定快照，默认为最近一次应用之前的状态 |
| `/bb snapshot list`、`/bb snapshot diff <引用> [引用]` | 查看快照与快照之间的差异 |
| `/bb admin list`、`/bb admin add <身份>`、`/bb admin remove <身份>` | 管理员管理；回复某人的消息发送 `/bb admin add` 可以直接添加这个人 |
| `/bb classifier status\|on\|off` | 查看、打开、关闭 classifier |
| `/bb classifier allow\|deny\|clear <工具模式…>` | 管理 classifier 的放行与拒绝规则 |

## 命令行

在运行中的容器里执行 `docker exec bearbot bearbotd <命令>`：

| 命令 | 作用 |
|---|---|
| `status` | 全部进程、平台、会话、快照、故障与维护状态 |
| `logs <daemon\|gateway\|agent\|recovery:<故障id>> [-n 行数] [-f]` | 查看日志 |
| `admin token [--ttl 10m]` | 生成一次性管理员令牌 |
| `admin list`、`admin revoke <身份>` | 管理员管理 |
| `login` | 交互式登录模型服务，需要 `docker exec -it` |
| `restart` | 重启 agent 进程 |
| `incident list`、`incident show <id>` | 故障记录 |
| `repair [说明]`、`resume` | 维护状态下的处理 |
| `rollback [快照]` | 管理员回滚 |
| `snapshot list`、`snapshot diff <引用> [引用]` | 快照查看 |
| `python rebuild` | 镜像更换 Python 版本后重建虚拟环境，在 daemon 没有运行时用 `docker run --rm <挂载参数> bearbot python rebuild` 执行 |

## 本地开发

需要的软件：Go 1.27、Node.js 24、git、Python 3.14（`/usr/bin/python3.14`）、uv（`/usr/bin/uv`、`/usr/bin/uvx`），以及 gateway 控制台测试使用的 util-linux `script` 与 `tmux`。

```sh
npm ci
npm run build -w gateway
```

### 不使用 Docker 运行

`bearbotd` 有两个只用于本地测试的全局参数：`--root <目录>` 把所有路径放在这个目录之下，`--rootless` 让所有子进程以当前用户运行，不切换用户，不修改属主。目录的布局与需要建立的符号链接见 DESIGN.md 第 16.1 节，`daemon/internal/testenv` 按同样的方式为每个测试建立目录。

```sh
cd daemon && go build -o bin/bearbotd ./cmd/bearbotd
bin/bearbotd --root /tmp/bbroot --rootless run
bin/bearbotd --root /tmp/bbroot --rootless status     # 在另一个终端中执行命令行
```

没有 `--rootless` 时 `bearbotd` 必须以 root 运行。

### 测试

测试全部使用真实组件：真实的 bearbotd、gateway、pi 进程，以及 git、uv、npm，不使用 mock。在仓库根目录执行：

| 命令 | 内容 |
|---|---|
| `npm run build -w gateway` | 编译 gateway，daemon 的端到端测试需要它 |
| `npm run typecheck -w gateway` | gateway 与它的测试的类型检查 |
| `npm test -w gateway` | gateway 的测试 |
| `node_modules/.bin/tsc -p extensions/tsconfig.json`、`node_modules/.bin/tsc -p extensions/bearbot-recovery/tsconfig.json` | `bearbot-im`、SDK 与 recovery 扩展的类型检查 |
| `node_modules/.bin/vitest run extensions` | `bearbot-im` 与 recovery 扩展的测试 |
| `cd daemon && go vet ./... && go test -count=1 -timeout 40m ./...` | Go 的单元测试与端到端测试；端到端测试约需 11 分钟，超过 `go test` 默认的 10 分钟上限，所以必须加 `-timeout` |

需要外部服务的测试在缺少下面的环境变量时跳过：

| 服务 | 环境变量 |
|---|---|
| 模型服务（OpenAI 兼容接口） | `BEARBOT_TEST_OPENAI_BASE_URL`、`BEARBOT_TEST_OPENAI_API_KEY`、`BEARBOT_TEST_OPENAI_MODEL` |
| Telegram | `BEARBOT_TEST_TELEGRAM_BOT_TOKEN`、`BEARBOT_TEST_TELEGRAM_CHAT_ID` |
| QQ | `BEARBOT_TEST_QQ_APP_ID`、`BEARBOT_TEST_QQ_CLIENT_SECRET` |

## 尚未实现

- 企业微信适配器（计划放在 `packages/chat-adapter-wecom/`）。配置中 `[platforms.wecom] enabled = true` 时 daemon 拒绝启动。
