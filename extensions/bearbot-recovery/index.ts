import { readFileSync } from "node:fs";
import { request } from "node:http";
import { StringEnum } from "@earendil-works/pi-ai";
import {
	DEFAULT_MAX_BYTES,
	DEFAULT_MAX_LINES,
	type ExtensionAPI,
	formatSize,
	truncateHead,
	truncateTail,
} from "@earendil-works/pi-coding-agent";
import { type Static, type TSchema, Type } from "typebox";
import { Value } from "typebox/value";

const recoveryRules = readFileSync(new URL("./prompt/recovery.md", import.meta.url), "utf8");

const DEFAULT_LOG_LINES = 200;

const LogsParams = Type.Object({
	source: StringEnum(["agent", "gateway", "daemon"] as const, {
		description: "读取哪个组件的日志：agent 为 agent 进程的日志，gateway 为 gateway.log，daemon 为 daemon.log",
	}),
	kind: StringEnum(["stderr", "rpc", "exits"] as const, {
		description:
			"source 为 agent 时选择日志：stderr 为 agent 进程的标准错误输出，rpc 为 RPC 事件记录（extension_error、agent_start、agent_settled、出错的助手消息），exits 为每次进程退出的记录（退出码、信号、stderr 末尾）。gateway 与 daemon 各只有一个日志，kind 填 stderr",
	}),
	n: Type.Optional(Type.Integer({ minimum: 1, description: `读取最后多少行，默认 ${DEFAULT_LOG_LINES}` })),
});

const CheckParams = Type.Object({});

const SnapshotParams = Type.Object({
	action: StringEnum(["list", "diff", "restore"] as const, {
		description:
			"list：列出最近的快照引用，以及 last-good、runningSnapshot 与回滚的默认目标；diff：两个快照之间的 git diff --stat，只给 ref 时与它的父提交比较；restore：把重启生效路径恢复到 ref 指向的快照",
	}),
	ref: Type.Optional(
		Type.String({
			description:
				"快照引用：init、apply/<n>、incident/<故障id>/before、incident/<故障id>/attempt-<n>、rollback/<n>/before、rollback/<n>/after、resume/<n>、last-good，末尾可以加 ^ 表示父提交；也可以是 40 位提交哈希",
		}),
	),
	ref2: Type.Optional(Type.String({ description: "diff 的第二个快照引用" })),
	paths: Type.Optional(
		Type.Array(Type.String(), {
			description:
				"restore 时只恢复这些路径，路径相对 /bearbot，例如 workspace/.pi/extensions/foo.ts，不接受绝对路径；只能是重启生效路径（workspace/.pi/ 下的路径、profile/agent/settings.json、profile/agent/models.json）。不填时恢复全部重启生效路径",
		}),
	),
});

const ReportParams = Type.Object({
	status: StringEnum(["fixed", "cannot_fix"] as const, {
		description: "fixed：已经修复，并且 recovery_check 通过；cannot_fix：无法修复",
	}),
	summary: Type.String({
		minLength: 1,
		description: "故障的原因与所做的修改；把扩展移到 .disabled/ 时写明移动了什么、失去了哪些功能",
	}),
	changed_files: Type.Array(Type.String(), {
		description: "修改、新建、删除、移动、恢复过的文件的绝对路径；移动的文件同时列出原位置与新位置",
	}),
	lesson: Type.String({
		minLength: 1,
		description: "写给 agent 看的经验：错误的原因，以及以后编写扩展时怎样避免",
	}),
});

const ErrorBody = Type.Object({ error: Type.String() });
const LogsResult = Type.Object({ lines: Type.Array(Type.String()) });
const CheckResult = Type.Object({ id: Type.String(), passed: Type.Boolean(), reportPath: Type.String() });
const SnapshotTextResult = Type.Object({ text: Type.String() });
const SnapshotRestoreResult = Type.Object({ restored: Type.Array(Type.String()), text: Type.String() });
const ReportResult = Type.Object({ accepted: Type.Literal(true) });

type Method = "GET" | "POST";

interface HttpReply {
	status: number;
	body: string;
}

function requireEnv(name: string): string {
	const value = process.env[name];
	if (!value) {
		throw new Error(`bearbot-recovery 需要环境变量 ${name}`);
	}
	return value;
}

function exchange(
	socketPath: string,
	method: Method,
	path: string,
	payload: string | undefined,
	signal: AbortSignal | undefined,
): Promise<HttpReply> {
	return new Promise((resolve, reject) => {
		const headers: Record<string, string | number> = {};
		if (payload !== undefined) {
			headers["content-type"] = "application/json";
			headers["content-length"] = Buffer.byteLength(payload);
		}
		const req = request({ socketPath, method, path, headers, signal }, (res) => {
			const chunks: Buffer[] = [];
			res.on("data", (chunk: Buffer) => chunks.push(chunk));
			res.on("error", reject);
			res.on("end", () => {
				if (res.statusCode === undefined) {
					reject(new Error(`agent.sock 对 ${method} ${path} 的响应没有状态码`));
					return;
				}
				resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString("utf8") });
			});
		});
		req.on("error", reject);
		if (payload !== undefined) {
			req.write(payload);
		}
		req.end();
	});
}

function parseBody(reply: HttpReply, method: Method, path: string): unknown {
	try {
		return JSON.parse(reply.body);
	} catch (cause) {
		throw new Error(`daemon 对 ${method} ${path} 的响应（HTTP ${reply.status}）不是 JSON：${reply.body}`, { cause });
	}
}

async function callDaemon<T extends TSchema>(
	socketPath: string,
	method: Method,
	path: string,
	body: object | undefined,
	signal: AbortSignal | undefined,
	schema: T,
): Promise<Static<T>> {
	const reply = await exchange(socketPath, method, path, body === undefined ? undefined : JSON.stringify(body), signal);
	const parsed = parseBody(reply, method, path);
	if (reply.status >= 400) {
		const message = Value.Check(ErrorBody, parsed) ? parsed.error : reply.body;
		throw new Error(`daemon 拒绝了 ${method} ${path}（HTTP ${reply.status}）：${message}`);
	}
	if (!Value.Check(schema, parsed)) {
		throw new Error(`daemon 对 ${method} ${path} 的响应不符合约定：${reply.body}`);
	}
	return parsed;
}

function textResult(text: string) {
	return { content: [{ type: "text" as const, text }], details: undefined };
}

function limitTail(text: string): string {
	const truncation = truncateTail(text, { maxLines: DEFAULT_MAX_LINES, maxBytes: DEFAULT_MAX_BYTES });
	if (!truncation.truncated) {
		return text;
	}
	return `[输出超过 ${DEFAULT_MAX_LINES} 行或 ${formatSize(DEFAULT_MAX_BYTES)}，只显示最后 ${truncation.outputLines} 行，共 ${truncation.totalLines} 行]\n${truncation.content}`;
}

function limitHead(text: string, whereComplete: string): string {
	const truncation = truncateHead(text, { maxLines: DEFAULT_MAX_LINES, maxBytes: DEFAULT_MAX_BYTES });
	if (!truncation.truncated) {
		return text;
	}
	return `${truncation.content}\n[输出超过 ${DEFAULT_MAX_LINES} 行或 ${formatSize(DEFAULT_MAX_BYTES)}，只显示开头 ${truncation.outputLines} 行，共 ${truncation.totalLines} 行。${whereComplete}]`;
}

export default function bearbotRecovery(pi: ExtensionAPI): void {
	const socketPath = requireEnv("BEARBOT_AGENT_SOCKET");
	const incidentId = requireEnv("BEARBOT_INCIDENT_ID");
	const section = `${recoveryRules.trimEnd()}\n\n当前处理的故障：${incidentId}\n`;

	pi.on("before_agent_start", (event) => {
		event.systemPromptOptions.sections.bearbot_recovery = section;
	});

	pi.registerTool({
		name: "recovery_logs",
		label: "recovery logs",
		description: `读取 bearbot 的日志：agent 进程的 stderr、RPC 事件记录、退出记录，以及 gateway、daemon 的日志。返回最后 n 行；输出超过 ${DEFAULT_MAX_LINES} 行或 ${formatSize(DEFAULT_MAX_BYTES)} 时只保留最后的部分。`,
		promptSnippet: "读取 agent 进程、gateway、daemon 的日志",
		parameters: LogsParams,
		async execute(_toolCallId, params, signal) {
			const n = params.n ?? DEFAULT_LOG_LINES;
			const query = new URLSearchParams({ source: params.source, kind: params.kind, n: String(n) });
			const result = await callDaemon(socketPath, "GET", `/v1/logs?${query.toString()}`, undefined, signal, LogsResult);
			if (result.lines.length === 0) {
				return textResult(`没有日志记录（source=${params.source}，kind=${params.kind}）。`);
			}
			return textResult(limitTail(result.lines.join("\n")));
		},
	});

	pi.registerTool({
		name: "recovery_check",
		label: "recovery check",
		description:
			"执行与 agent 的 bearbot_check 相同的检查：位置检查、类型检查、试运行启动（以与正式运行相同的参数启动一次 agent 进程，确认全部扩展都能加载）。返回检查报告的 JSON。报告的 passed 为 true 才说明 agent 进程可以启动。",
		promptSnippet: "检查工作区的扩展与 profile，确认 agent 进程可以启动",
		parameters: CheckParams,
		async execute(_toolCallId, _params, signal) {
			const report = await callDaemon(socketPath, "POST", "/v1/check", {}, signal, CheckResult);
			const verdict = report.passed ? `检查通过（${report.id}）。` : `检查未通过（${report.id}）。`;
			const body = limitHead(JSON.stringify(report, null, 2), `完整报告：${report.reportPath}`);
			return textResult(`${verdict}\n${body}`);
		},
	});

	pi.registerTool({
		name: "recovery_snapshot",
		label: "recovery snapshot",
		description:
			"查看与恢复快照。list 列出快照，diff 比较两个快照（git diff --stat），restore 把重启生效路径（workspace/.pi/**、profile/agent/settings.json、profile/agent/models.json）恢复到指定快照；AGENTS.md 与 CLASSIFIER.md 不会被恢复。",
		promptSnippet: "列出、比较快照，把重启生效路径恢复到某个快照",
		parameters: SnapshotParams,
		async execute(_toolCallId, params, signal) {
			const body = { action: params.action, ref: params.ref, ref2: params.ref2, paths: params.paths };
			if (params.action === "restore") {
				const result = await callDaemon(socketPath, "POST", "/v1/snapshot", body, signal, SnapshotRestoreResult);
				const restored =
					result.restored.length === 0
						? "没有文件被恢复。"
						: `恢复的文件（${result.restored.length} 个）：\n${result.restored.join("\n")}`;
				return textResult(limitHead(`${result.text}\n\n${restored}`, "可以用 git 命令查看快照仓库中的完整内容。"));
			}
			const result = await callDaemon(socketPath, "POST", "/v1/snapshot", body, signal, SnapshotTextResult);
			return textResult(limitHead(result.text, "可以用 git 命令查看快照仓库中的完整内容。"));
		},
	});

	pi.registerTool({
		name: "recovery_report",
		label: "recovery report",
		description:
			"向 daemon 提交修复报告，每一轮修复以它结束。status 为 fixed 时，daemon 提交快照、重新检查并启动 agent 进程；为 cannot_fix 时，bot 进入维护状态，等待管理员处理。只在 recovery_check 通过之后报告 fixed。",
		promptSnippet: "提交修复报告（fixed 或 cannot_fix）",
		parameters: ReportParams,
		async execute(_toolCallId, params, signal) {
			const body = { status: params.status, summary: params.summary, changed_files: params.changed_files, lesson: params.lesson };
			await callDaemon(socketPath, "POST", "/v1/recovery/report", body, signal, ReportResult);
			if (params.status === "fixed") {
				return textResult(
					"daemon 已收到修复报告，接下来由 daemon 提交快照、重新检查并启动 agent 进程。本轮到此结束，不要再修改任何文件。",
				);
			}
			return textResult("daemon 已收到无法修复的报告，bot 将进入维护状态，等待管理员处理。本轮到此结束，不要再修改任何文件。");
		},
	});
}
