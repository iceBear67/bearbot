import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { loadSkillsFromDir } from "@earendil-works/pi-coding-agent";
import { afterEach, describe, expect, test } from "vitest";
import { commandNames, PiRpc, repoRoot } from "./pi-rpc.ts";

const skillsDir = join(repoRoot, "skills");
const skillNames = ["bearbot-extension-dev", "bearbot-middleware", "bearbot-recovery"];

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

function tempRoot(): string {
	const root = mkdtempSync("/tmp/bb");
	roots.push(root);
	return root;
}

function frontmatterDescription(name: string): string {
	const text = readFileSync(join(skillsDir, name, "SKILL.md"), "utf8");
	const match = /^---\nname: (.+)\ndescription: (.+)\n---\n/.exec(text);
	if (!match) {
		throw new Error(`${name}/SKILL.md does not start with a name/description frontmatter`);
	}
	expect(match[1]).toBe(name);
	return match[2];
}

describe("自带技能", () => {
	test("pi 的技能加载器读取三个技能，没有任何诊断", () => {
		const result = loadSkillsFromDir({ dir: skillsDir, source: "test" });
		expect(result.diagnostics).toEqual([]);
		expect(result.skills.map((skill) => skill.name).sort()).toEqual(skillNames);
		for (const skill of result.skills) {
			expect(skill.filePath).toBe(join(skillsDir, skill.name, "SKILL.md"));
			expect(skill.description).toBe(frontmatterDescription(skill.name));
			expect(skill.description.length).toBeLessThanOrEqual(1024);
			expect(skill.disableModelInvocation).toBe(false);
		}
	});

	test.each(skillNames)("pi --skill 加载 %s，RPC get_commands 列出 skill:<名称>", async (name) => {
		const root = tempRoot();
		const agentDir = join(root, "agent");
		mkdirSync(agentDir);
		const pi = new PiRpc(["--no-session", "-ne", "-ns", "--skill", join(skillsDir, name), "-np", "-nc", "-na"], {
			cwd: root,
			env: { PATH: process.env.PATH as string, HOME: root, PI_CODING_AGENT_DIR: agentDir, PI_SKIP_VERSION_CHECK: "1" },
		});
		processes.push(pi);
		const response = await pi.request({ id: "commands", type: "get_commands" }, 20_000);
		expect(response.success).toBe(true);
		expect(commandNames(response)).toEqual([`skill:${name}`]);
		const [command] = (response.data as { commands: { description: string; source: string }[] }).commands;
		expect(command.source).toBe("skill");
		expect(command.description).toBe(frontmatterDescription(name));
		expect(await pi.close()).toEqual({ code: 0, signal: null });
		expect(pi.stderr).toBe("");
	}, 30_000);
});

function toolExtension(tools: string[], commands: string[], reportTools: boolean): string {
	return [
		'import { Type } from "typebox";',
		"export default function (pi: any) {",
		...tools.map(
			(tool) =>
				`  pi.registerTool({ name: "${tool}", label: "${tool}", description: "${tool}", parameters: Type.Object({}), async execute() { return { content: [{ type: "text", text: "x" }], details: undefined }; } });`,
		),
		...commands.map((command) => `  pi.registerCommand("${command}", { description: "${command}", handler: async () => {} });`),
		...(reportTools
			? [
					'  pi.on("session_start", (_event: unknown, ctx: any) => {',
					'    ctx.ui.notify(JSON.stringify(pi.getAllTools().map((tool: any) => [tool.name, tool.sourceInfo.path])), "info");',
					"  });",
				]
			: []),
		"}",
		"",
	].join("\n");
}

// pi started like the agent process (built-in extensions enabled, explicit tool allowlist), without bearbot-im.
function startPi(root: string, extensions: string[]): PiRpc {
	const agentDir = join(root, "agent");
	mkdirSync(agentDir);
	const pi = new PiRpc(["--no-session", "--tools", "+grep,+find,+ls", ...extensions.flatMap((path) => ["-e", path])], {
		cwd: root,
		env: { PATH: process.env.PATH as string, HOME: root, PI_CODING_AGENT_DIR: agentDir, PI_SKIP_VERSION_CHECK: "1" },
	});
	processes.push(pi);
	return pi;
}

describe("技能 bearbot-extension-dev 中工具同名的规则", () => {
	test("两个扩展注册同名工具时 pi 以退出码 1 退出，stderr 给出另一个扩展的路径", async () => {
		const root = tempRoot();
		const first = join(root, "first.ts");
		const second = join(root, "second.ts");
		writeFileSync(first, toolExtension(["weather_today"], [], false));
		writeFileSync(second, toolExtension(["weather_today"], [], false));
		const pi = startPi(root, [first, second]);
		expect(await pi.exited).toEqual({ code: 1, signal: null });
		expect(pi.stderr).toContain(`Tool "weather_today" conflicts with ${first}`);
	}, 30_000);

	test("与内置工具同名的扩展工具替换内置工具，codemode、tool_search 与命令 mcp 使 pi 不加载对应的内置扩展，启动照常成功", async () => {
		const root = tempRoot();
		const override = join(root, "override.ts");
		writeFileSync(override, toolExtension(["read", "bash", "codemode", "tool_search"], ["mcp"], true));
		const pi = startPi(root, [override]);
		const notice = await pi.waitFor((record) => record.type === "extension_ui_request" && record.method === "notify", 20_000);
		const owners = new Map(JSON.parse(notice.message as string) as [string, string][]);
		for (const tool of ["read", "bash", "codemode", "tool_search"]) {
			expect(owners.get(tool)).toBe(override);
		}
		expect(owners.get("edit")).toBe("builtin:edit");
		const state = await pi.request({ id: "state", type: "get_state" }, 20_000);
		expect(state.success).toBe(true);
		const commands = await pi.request({ id: "commands", type: "get_commands" }, 20_000);
		const mcp = (commands.data as { commands: { name: string; sourceInfo: { path: string } }[] }).commands.filter((command) =>
			command.name.startsWith("mcp"),
		);
		expect(mcp).toEqual([expect.objectContaining({ name: "mcp", sourceInfo: expect.objectContaining({ path: override }) })]);
		expect(await pi.close()).toEqual({ code: 0, signal: null });
		for (const builtin of ["codemode", "tool-search", "mcp"]) {
			expect(pi.stderr).toContain(`Warning: Extension package "builtin:${builtin}": Extension ${override} registers`);
		}
		expect(pi.stderr).not.toContain("conflicts with");
	}, 30_000);
});
