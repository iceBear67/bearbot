import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { commandNames, PiRpc, repoRoot } from "./pi-rpc.ts";

const recoveryExt = join(repoRoot, "extensions", "bearbot-recovery");
const recoverySkill = join(repoRoot, "skills", "bearbot-recovery");
const recoveryProfileTemplate = join(repoRoot, "recovery-profile", "settings.json");

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

// Environment of contracts section 2.3 without [recovery.env] entries.
function recoveryEnvironment(layout: RecoveryLayout, incidentId: string): Record<string, string> {
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
	return env;
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
});
