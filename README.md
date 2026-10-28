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

推送 `v*` 标签（例如 `v1.2.3`）时，GitHub Actions（`.github/workflows/docker.yml`）用默认参数构建 `linux/amd64` 与 `linux/arm64` 镜像，发布为 `ghcr.io/<所有者>/<仓库名>`，标签为 `1.2.3`、`1.2` 与 `latest`（预发布版本不更新 `latest`）。

## 快速上手（Getting Started）

bearbot 支持三种运行与调试方式：
1. **Docker Compose 本地部署（推荐）**：使用仓库预置的 [docker-compose.yml](file:///home/icybear/GolandProjects/bearbot/docker-compose.yml) 与 `local-test/` 挂载目录，开箱即用。
2. **Docker 命令行部署**：适合自定义路径与生产环境部署。
3. **终端控制台调试模式（Console）**：零 IM 门槛，直接在终端作为内置管理员与 agent 交互。

---

### 1. 准备挂载目录与权限

容器运行时包含三个持久化挂载点。启动前**必须确保宿主机上这三个目录均已建立**：

| 挂载点 | 宿主机对应（示例） | 职责与内容 |
|---|---|---|
| `/bearbot/profile` | `./local-test/profile` 或 `/srv/bearbot/profile` | Agent 的 profile（凭据 `auth.json`、模型定义 `models.json`、会话记录 `sessions/`）、Python 虚拟环境、安装的二进制可执行文件 `bin/`、日志与 Git 快照仓库。 |
| `/bearbot/workspace` | `./local-test/workspace` 或 `/srv/bearbot/workspace` | Agent 的工作区：记忆 [AGENTS.md](file:///home/icybear/GolandProjects/bearbot/local-test/workspace/AGENTS.md)、Classifier 规则 [CLASSIFIER.md](file:///home/icybear/GolandProjects/bearbot/local-test/workspace/CLASSIFIER.md)、自扩展代码与配置 `.pi/`（`extensions/`、`skills/`、`mcp.json`）、聊天收到的文件 `inbox/`。首次启动为空时 daemon 会自动初始化模板。 |
| `/bearbot/config` | `./local-test/config` 或 `/srv/bearbot/config` | 核心配置文件 `bearbot.toml`、可选的 recovery profile 覆盖配置、维护脚本 hooks。 |

#### 初始化目录结构
在仓库根目录下运行：

```sh
mkdir -p local-test/profile local-test/workspace local-test/config
cp config/bearbot.example.toml local-test/config/bearbot.toml
```

> [!NOTE]
> 容器内进程分别以 `root`（daemon）、`bearbot`（Agent/Recovery，默认 UID 1000）和 `bearbot-gw`（Gateway，默认 UID 1001）运行。容器启动时，daemon 会自动检查并修正挂载点下各子目录的属主与权限。

---

### 2. 配置环境变量（.env）

复制项目示例并配置 `.env` 文件（[docker-compose.yml](file:///home/icybear/GolandProjects/bearbot/docker-compose.yml) 会自动读取）：

```sh
cp .env.example .env
```

在 `.env` 中按需填入模型 API Key 与聊天平台凭据：

```dotenv
# ==============================
# 模型服务凭据
# ==============================
# 方式 A：使用 Anthropic 官方模型服务
ANTHROPIC_API_KEY=sk-ant-api03-...
BEARBOT_RECOVERY_API_KEY=sk-ant-api03-...

# 方式 B：如使用第三方/OpenAI 兼容模型（如 DeepSeek、Kimi、GLM 等），可在 .env 中定义自定义变量：
# OPENAI_API_KEY=sk-...

# ==============================
# IM 平台凭据（按需配置）
# ==============================
# Telegram 机器人令牌（从 @BotFather 获取）
TELEGRAM_BOT_TOKEN=1234567890:ABCdefGhIJKlmNoPQRsTUVwxyZ_example

# QQ 官方机器人凭据（从 QQ 开放平台获取）
QQ_BOT_APP_ID=102030405
QQ_BOT_CLIENT_SECRET=abcdef1234567890abcdef1234567890
```

---

### 3. 配置模型服务

Bearbot 由三部分依赖模型的能力组成：**Agent 主体**、**Classifier（安全分类器）** 与 **Recovery（故障修复 Agent）**。

#### 选项 A：使用官方 Anthropic 服务（默认）
直接在 `.env` 中提供 `ANTHROPIC_API_KEY` 和 `BEARBOT_RECOVERY_API_KEY`，在 `bearbot.toml` 中配置：
```toml
[agent.env]
ANTHROPIC_API_KEY = "${ANTHROPIC_API_KEY}"

[classifier]
enabled = true
model = "anthropic/claude-haiku-4-5-20251001"

[recovery.env]
ANTHROPIC_API_KEY = "${BEARBOT_RECOVERY_API_KEY}"
```

#### 选项 B：使用 OpenAI 兼容自定义服务（如 DeepSeek、GLM、Kimi 等）
参考 `local-test` 的多模型配置实践：

1. **配置 Agent 模型服务**：在挂载目录的 `profile/agent/models.json` 中配置 provider（支持 `openai-completions` 接口）：
   ```json
   {
     "providers": {
       "deepseek": {
         "name": "DeepSeek API",
         "baseUrl": "https://api.deepseek.com/v1",
         "api": "openai-completions",
         "apiKey": "sk-your-key-here",
         "models": [
           {
             "id": "deepseek-chat",
             "name": "DeepSeek-V3",
             "contextWindow": 65536,
             "maxTokens": 8192
           }
         ]
       }
     }
   }
   ```
2. **指定 Agent 默认使用的模型**：在工作区创建 `workspace/.pi/settings.json`：
   ```json
   {
     "defaultProvider": "deepseek",
     "defaultModel": "deepseek-chat"
   }
   ```
3. **配置 Classifier 模型**：在 `local-test/config/bearbot.toml` 中将 `[classifier] model` 指向自定义模型的 `<服务ID>/<模型ID>`：
   ```toml
   [classifier]
   enabled = true
   model = "deepseek/deepseek-chat"
   ```
4. **配置 Recovery 模型（可选）**：Recovery 使用独立的 profile，若不使用 Anthropic，可在 `config/recovery/` 下放置 `settings.json`、`models.json` 与 `auth.json`，其中 `settings.json` 必须保留 `defaultProvider`。

#### 选项 C：交互式登录模型服务
容器运行后，可随时进入交互式登录命令行：
```sh
docker exec -it bearbot bearbotd login
```
登录成功后凭据会自动写入 `profile/agent/auth.json`。

---

### 4. 启动与管理

#### 方式一：使用 Docker Compose 启动（推荐）

```sh
# 构建镜像并后台启动
docker compose up -d --build

# 查看运行状态
docker exec bearbot bearbotd status

# 实时查看日志
docker compose logs -f bearbot
# 或者使用 bearbotd 专用日志命令查看具体组件
docker exec bearbot bearbotd logs agent -f
docker exec bearbot bearbotd logs gateway -f
```

#### 方式二：使用 `docker run` 直接运行

```sh
docker run -d --name bearbot --stop-timeout 120 --tmpfs /run/bearbot \
  -v $(pwd)/local-test/profile:/bearbot/profile \
  -v $(pwd)/local-test/workspace:/bearbot/workspace \
  -v $(pwd)/local-test/config:/bearbot/config \
  --env-file .env \
  bearbot
```

> [!IMPORTANT]
> - `--tmpfs /run/bearbot` 是必需参数，所有跨进程 Unix Socket 通信均位于此内存文件系统中。
> - `--stop-timeout` 必须设置且大于 `[daemon] shutdown_grace`（默认 90 秒），以确保优雅停止 agent 当前正在执行的回合。

---

### 5. 成为管理员并绑定聊天频道

服务启动后，Gateway 会连接配置的 IM 平台（Telegram / QQ）。Bearbot 采用基于令牌的管理员授权和显式频道绑定机制：

1. **生成一次性管理员令牌**：
   ```sh
   docker exec bearbot bearbotd admin token
   ```
   终端会输出形如 `tok-xxxxxxxxxxxxxxxx` 的令牌（默认 10 分钟内有效）。

2. **领取管理员身份**：
   - 打开 Telegram 或 QQ，在私聊中将令牌直接发送给机器人（私聊最可靠）。
   - 机器人核销成功后会回复：`已成为 bearbot 管理员`。之后所有的系统通知（故障报告、修复通知、回滚提醒）都会发送到该聊天。

3. **绑定工作频道**：
   - 将机器人拉入群聊（或在私聊窗口中），发送绑定指令：
     ```text
     /bb attach
     ```
   - `/bb attach` 支持以下参数调整触发与回复行为：
     - `--trigger mention`（默认群聊模式：仅在 @机器人 时触发）
     - `--trigger all`（处理频道内的所有消息，适用于私聊或专属协作群）
     - `--trigger prefix --prefix <前缀>`（匹配指定前缀的消息才触发）
     - `--reply final|each`（`final` 只回复最终结果，`each` 每轮思考均回复）

> [!TIP]
> - **Telegram 权限提示**：Telegram 群组中机器人要接收非 `/` 开头的消息，必须在 [@BotFather](https://t.me/BotFather) 中通过 `/setprivacy` 将 Privacy mode 设为 `Disable`，或者直接将机器人提升为群管理员。
> - **QQ 平台提示**：QQ 群与 QQ 单聊在官方平台下用户的 OpenID 不同（属于两个身份）。如需在两处都作为管理员，可分别使用 `admin token` 领取，或在已认证的管理端通过 `/bb admin add <身份ID>` 添加。

---

### 6. 控制台调试（Console 模式）

如果你希望在本地无 IM 账号的情况下快速验证 Agent 功能，可以使用内置的终端控制台。控制台用户拥有内置超级管理员权限（`console:operator`），无需生成或领取管理员令牌。

1. 在 `bearbot.toml` 中确认控制台开启：
   ```toml
   [platforms.console]
   enabled = true
   ```
2. 附加到运行中的容器终端（`docker-compose.yml` 默认已配置 `stdin_open: true` 与 `tty: true`）：
   ```sh
   docker attach bearbot
   ```
3. 按一下 `Enter` 键即可看到提示符 `bearbot> `：
   - 直接输入文字并回车即可向 Agent 发送消息。
   - 输入 `:help` 查看控制台命令列表。
   - 输入 `:file <相对工作区的文件路径> [附带文字]` 向 Agent 发送工作区文件。
   - 输入 `::<文字>` 发送以冒号开头的普通消息。
   - 在行末输入 `\` 可换行输入多行消息。
   - 按快捷键 `Ctrl-P` 然后 `Ctrl-Q` 可以脱离（detach）控制台，容器继续在后台运行。

---

### 7. Agent 的自我演进与验证

进入聊天或控制台后，你就可以指导 Agent 自我扩展。例如：
- 发送：`“请阅读 docs/ 目录下的开发指南，并为自己编写一个定时天气推送的扩展，放在 .pi/extensions/ 目录下”`
- Agent 会调用工具编辑 `.pi/extensions/`。
- 编写完成后，Agent 触发 `bearbot_check` 执行类型检查、位置检查与无害试运行。
- 检查通过后调用 `bearbot_apply` 提交 Git 快照并平滑重启。
- 如果新扩展导致语法错误或崩溃，Bearbot 的独立 Recovery Agent 会接管并自动诊断修复；若修复未达预期，管理员可随时发送 `/bb rollback` 回滚到最近一次稳定快照。


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
| `!bb <问题>` | 在不打断 agent 的情况下询问它正在做什么：一个单独的会话根据当前会话的内容回答，主会话看不到这次问答 |

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

## 常见问题（FAQ）

### 挂载目录迁移到另一台机器后，容器启动了却没有任何反应

用 `docker exec bearbot bearbotd logs gateway -n 50` 查看 gateway 的日志，如果其中有 `SQLITE_READONLY`、`EACCES` 等错误，`bearbotd logs daemon` 中反复出现 `gateway exited unexpectedly`，原因就是挂载目录中文件的属主不正确。用普通用户解压、用 root 解压但没有加 `--numeric-owner`、或者服务器的 Docker 使用 rootless 模式或 userns-remap，都会让文件的数字属主与容器中的 `bearbot`（UID 1000）、`bearbot-gw`（UID 1001）不一致。daemon 启动时只修正各个目录本身的属主与权限，目录中的文件保持原样；gateway 无法写入 `gateway.db` 而不断退出，daemon 一直等待 gateway 连接，不会启动 agent。daemon 的日志只写入 `profile/bearbot/logs/daemon.log`，所以 `docker compose logs` 中看不到任何输出。

容器运行时，在仓库根目录执行 [CLEAN.sh](CLEAN.sh)：

```sh
./CLEAN.sh
```

它在名为 `bearbot` 的容器中按 DESIGN.md 第 4.3、4.4 节的目录表恢复三个挂载点中全部文件的属主（不跟随符号链接），为工作区恢复 `bearbot` 组的读写权限与目录的 setgid 位，然后重启容器。`profile/` 中的文件权限保持不变，`auth.json` 等凭据文件仍然只有 `bearbot` 可以读取。重启后 `bearbotd logs daemon` 中出现 `gateway ready` 与 `agent ready` 即恢复正常。

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
