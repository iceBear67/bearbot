import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { commandNames, PiRpc, type RpcRecord, repoRoot } from "./pi-rpc.ts";

const recoveryExt = join(repoRoot, "extensions", "bearbot-recovery");
const recoverySkill = join(repoRoot, "skills", "bearbot-recovery");
const recoveryProfileTemplate = join(repoRoot, "recovery-profile", "settings.json");
const recoveryRules = readFileSync(join(recoveryExt, "prompt", "recovery.md"), "utf8");

const modelBaseUrl = process.env.BEARBOT_TEST_OPENAI_BASE_URL;
const modelApiKey = process.env.BEARBOT_TEST_OPENAI_API_KEY;
const modelId = process.env.BEARBOT_TEST_OPENAI_MODEL;
const hasModel = Boolean(modelBaseUrl && modelApiKey && modelId);

const DISTRACTOR_MARKERS = [
	"CWD_AGENTS_MARKER",
	"WORKSPACE_AGENTS_MARKER",
	"PROFILE_AGENTS_MARKER",
	"distract-skill",
	"global-skill",
	"home-skill",
	"ancestor-skill",
	"distract_tool",
	"global_tool",
];

interface RecoveryLayout {
	root: string;
	cwd: string;
	home: string;
	agentDir: string;
	sessionsDir: string;
	agentSock: string;
	snapshotsGit: string;
}

const roots: string[] = [];
const processes: PiRpc[] = [];

afterEach(() => {
	for (const pi of processes.splice(0)) {
		pi.kill();
	}
	for (const root of roots.splice(0)) {
		rmSync(root, { recursive: true, force: true });
	}
});

function write(path: string, content: string): void {
	mkdirSync(dirname(path), { recursive: true });
	writeFileSync(path, content);
}

function extensionSource(command: string, tool: string): string {
	return [
		'import { Type } from "typebox";',
		"export default function (pi: any) {",
		`  pi.registerCommand("${command}", { handler: async () => {} });`,
		`  pi.registerTool({ name: "${tool}", label: "${tool}", description: "${tool}", parameters: Type.Object({}),`,
		'    async execute() { return { content: [{ type: "text", text: "x" }], details: undefined }; } });',
		"}",
		"",
	].join("\n");
}

function skillSource(name: string): string {
	return `---\nname: ${name}\ndescription: ${name} must not be loaded into recovery.\n---\n\n${name}\n`;
}

// Builds the directories of contracts section 1.1 under a fresh root prefix, the recovery agent dir prepared the way
// DESIGN 10.1 describes (template copied), and content that a recovery process must not load.
function makeLayout(): RecoveryLayout {
	const root = mkdtempSync("/tmp/bb");
	roots.push(root);
	const layout: RecoveryLayout = {
		root,
		cwd: join(root, "bearbot"),
		home: join(root, "home", "bearbot"),
		agentDir: join(root, "run", "bearbot", "recovery", "agent"),
		sessionsDir: join(root, "bearbot", "profile", "bearbot", "recovery-sessions"),
		agentSock: join(root, "run", "bearbot", "agent.sock"),
		snapshotsGit: join(root, "bearbot", "profile", "bearbot", "snapshots.git"),
	};
	mkdirSync(layout.agentDir, { recursive: true });
	mkdirSync(layout.sessionsDir, { recursive: true });
	mkdirSync(layout.home, { recursive: true });
	copyFileSync(recoveryProfileTemplate, join(layout.agentDir, "settings.json"));

	write(join(layout.agentDir, "AGENTS.md"), "PROFILE_AGENTS_MARKER\n");
	write(join(layout.agentDir, "extensions", "global.ts"), extensionSource("globalcmd", "global_tool"));
	write(join(layout.agentDir, "skills", "global-skill", "SKILL.md"), skillSource("global-skill"));
	write(join(layout.agentDir, "prompts", "global-prompt.md"), "---\ndescription: global prompt\n---\nGLOBAL_PROMPT\n");

	write(join(layout.cwd, "AGENTS.md"), "CWD_AGENTS_MARKER\n");
	write(join(layout.cwd, ".pi", "extensions", "distract.ts"), extensionSource("distractcmd", "distract_tool"));
	write(join(layout.cwd, ".pi", "skills", "distract-skill", "SKILL.md"), skillSource("distract-skill"));
	write(join(layout.cwd, ".pi", "prompts", "distract-prompt.md"), "---\ndescription: distract prompt\n---\nDISTRACT_PROMPT\n");
	write(join(layout.cwd, "workspace", "AGENTS.md"), "WORKSPACE_AGENTS_MARKER\n");
	write(join(layout.cwd, "workspace", ".pi", "extensions", "workspace.ts"), extensionSource("workspacecmd", "workspace_tool"));
	write(join(layout.home, ".agents", "skills", "home-skill", "SKILL.md"), skillSource("home-skill"));
	write(join(root, ".agents", "skills", "ancestor-skill", "SKILL.md"), skillSource("ancestor-skill"));
	return layout;
}

function childPath(layout: RecoveryLayout): string {
	const entries = [
		join(layout.root, "bearbot", "profile", "bin"),
		join(layout.root, "bearbot", "profile", "python", "bin"),
		join(layout.root, "usr", "local", "bin"),
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		dirname(process.execPath),
	];
	return [...new Set(entries)].join(":");
}

// Environment of contracts section 2.3; recoveryEnv plays the role of the [recovery.env] entries.
function recoveryEnvironment(layout: RecoveryLayout, incidentId: string, recoveryEnv: Record<string, string> = {}): Record<string, string> {
	const env: Record<string, string> = {
		HOME: layout.home,
		PATH: childPath(layout),
		VIRTUAL_ENV: join(layout.root, "bearbot", "profile", "python"),
		UV_LINK_MODE: "copy",
		PI_SKIP_VERSION_CHECK: "1",
		PI_CODING_AGENT_DIR: layout.agentDir,
		BEARBOT_AGENT_SOCKET: layout.agentSock,
		BEARBOT_INCIDENT_ID: incidentId,
		GIT_CONFIG_COUNT: "1",
		GIT_CONFIG_KEY_0: "safe.directory",
		GIT_CONFIG_VALUE_0: layout.snapshotsGit,
	};
	if (process.env.TZ) env.TZ = process.env.TZ;
	if (process.env.LANG) env.LANG = process.env.LANG;
	return { ...env, ...recoveryEnv };
}

function recoveryArgs(layout: RecoveryLayout, incidentId: string): string[] {
	return [
		"-ne",
		"-e",
		recoveryExt,
		"--tools",
		"+grep,+find,+ls",
		"-ns",
		"--skill",
		recoverySkill,
		"-np",
		"-nc",
		"-na",
		"--session-dir",
		layout.sessionsDir,
		"--session-id",
		incidentId,
	];
}

function startRecovery(layout: RecoveryLayout, env: Record<string, string>, incidentId: string): PiRpc {
	const pi = new PiRpc(recoveryArgs(layout, incidentId), { cwd: layout.cwd, env });
	processes.push(pi);
	return pi;
}

describe("recovery 进程（contracts 2.3 的启动参数）", () => {
	test("只加载 bearbot-recovery 扩展与 bearbot-recovery 技能", async () => {
		const layout = makeLayout();
		const incidentId = "inc-20261027083000";
		const pi = startRecovery(layout, recoveryEnvironment(layout, incidentId), incidentId);

		const state = await pi.request({ id: "boot", type: "get_state" }, 20_000);
		expect(state.success).toBe(true);
		const stateData = state.data as { sessionId: string; sessionFile: string; isStreaming: boolean };
		expect(stateData.sessionId).toBe(incidentId);
		expect(stateData.sessionFile.startsWith(`${layout.sessionsDir}/`)).toBe(true);
		expect(stateData.sessionFile.endsWith(`_${incidentId}.jsonl`)).toBe(true);
		expect(stateData.isStreaming).toBe(false);

		const commands = await pi.request({ id: "commands", type: "get_commands" }, 10_000);
		expect(commands.success).toBe(true);
		expect(commandNames(commands)).toEqual(["skill:bearbot-recovery"]);
		const [skill] = (commands.data as { commands: { source: string; sourceInfo: { path: string } }[] }).commands;
		expect(skill.source).toBe("skill");
		expect(skill.sourceInfo.path).toBe(join(recoverySkill, "SKILL.md"));

		expect(pi.records.filter((record) => record.type === "extension_error")).toEqual([]);
		const exit = await pi.close();
		expect(exit).toEqual({ code: 0, signal: null });
		expect(pi.stderr.trim()).toBe(`Warning: No project session found with id '${incidentId}'; creating a new session with that id.`);
	}, 30_000);

	test.each(["BEARBOT_AGENT_SOCKET", "BEARBOT_INCIDENT_ID"])("缺少 %s 时 recovery 扩展加载失败，进程以 1 退出", async (name) => {
		const layout = makeLayout();
		const incidentId = "inc-20261027083001";
		const env = recoveryEnvironment(layout, incidentId);
		delete env[name];
		const pi = startRecovery(layout, env, incidentId);
		pi.send({ id: "boot", type: "get_state" });

		const exit = await pi.exited;
		expect(exit).toEqual({ code: 1, signal: null });
		expect(pi.records).toEqual([]);
		expect(pi.stderr).toContain(`Failed to load extension "${join(recoveryExt, "index.ts")}"`);
		expect(pi.stderr).toContain(`bearbot-recovery 需要环境变量 ${name}`);
	}, 30_000);

	test.skipIf(!hasModel)(
		"一轮中的系统提示带有 recovery 规则、全部七个内置工具与四个 recovery 工具（需要 BEARBOT_TEST_OPENAI_BASE_URL、BEARBOT_TEST_OPENAI_API_KEY、BEARBOT_TEST_OPENAI_MODEL）",
		async () => {
			const layout = makeLayout();
			const incidentId = "inc-20261027083002";
			// The daemon copies /bearbot/config/recovery/{settings,models}.json over the template (DESIGN 10.1).
			const template = JSON.parse(readFileSync(recoveryProfileTemplate, "utf8")) as Record<string, unknown>;
			writeFileSync(
				join(layout.agentDir, "settings.json"),
				JSON.stringify({ ...template, defaultProvider: "testprovider", defaultModel: modelId }, null, 2),
			);
			writeFileSync(
				join(layout.agentDir, "models.json"),
				JSON.stringify({
					providers: {
						testprovider: {
							api: "openai-completions",
							baseUrl: modelBaseUrl,
							apiKey: "${BEARBOT_TEST_OPENAI_API_KEY}",
							models: [{ id: modelId }],
						},
					},
				}),
			);
			const env = recoveryEnvironment(layout, incidentId, { BEARBOT_TEST_OPENAI_API_KEY: modelApiKey as string });
			const pi = startRecovery(layout, env, incidentId);

			const state = await pi.request({ id: "boot", type: "get_state" }, 20_000);
			const model = (state.data as { model: { provider: string; id: string } }).model;
			expect(`${model.provider}/${model.id}`).toBe(`testprovider/${modelId}`);

			const prompt = await pi.request(
				{ id: "prompt", type: "prompt", message: "这是一次连通性检查。不要调用任何工具，只回复两个字：完成。" },
				60_000,
			);
			expect(prompt.success).toBe(true);
			const system = await pi.waitFor(
				(record) => record.type === "message_start" && (record.message as { role: string }).role === "system",
				60_000,
			);
			const message = system.message as { sections: Record<string, string>; toolsAdded: { name: string }[] };
			expect(message.toolsAdded.map((tool) => tool.name).sort()).toEqual(
				[
					"bash",
					"edit",
					"find",
					"grep",
					"ls",
					"read",
					"recovery_check",
					"recovery_logs",
					"recovery_report",
					"recovery_snapshot",
					"write",
				].sort(),
			);
			const section = message.sections.bearbot_recovery;
			expect(section).toContain(recoveryRules.trim());
			expect(section).toContain(`当前处理的故障：${incidentId}`);
			expect(message.sections.skills).toContain("<name>bearbot-recovery</name>");
			const serialized = JSON.stringify(message);
			for (const marker of DISTRACTOR_MARKERS) {
				expect(serialized).not.toContain(marker);
			}

			await pi.waitFor((record) => record.type === "agent_settled", 120_000);
			const assistant = pi.records.filter(
				(record): record is RpcRecord & { message: { role: string; stopReason: string; errorMessage?: string } } =>
					record.type === "message_end" && (record.message as { role: string }).role === "assistant",
			);
			expect(assistant.length).toBeGreaterThan(0);
			const last = assistant[assistant.length - 1].message;
			expect(last.stopReason, last.errorMessage).not.toBe("error");
			expect(await pi.close()).toEqual({ code: 0, signal: null });
		},
		180_000,
	);
});
