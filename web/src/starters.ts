import askSource from "../../examples/definitions/smoke.yaml?raw";
import buildSource from "../../examples/definitions/plan-build-test.yaml?raw";

export type TaskMode = "ask" | "build" | "auto";
export const automaticGoal = "Inspect this project, choose one useful improvement supported by the code, write a concrete directive and implementation plan, build it, and verify it with the existing tests. Keep the change focused. Do not add paid services or change deployment, authentication, secrets, or security policy. Do not weaken tests. Report the chosen improvement, changes, and verification.";
export const defaultModel = "gpt-6.1-sol";
export const taskSteps = { ask: ["scout"], build: ["plan", "build", "test"], auto: ["plan", "build", "test"] };

export async function starterSource(mode: TaskMode, model: string, testCommand = ""): Promise<string> {
  let source = (mode === "ask" ? askSource : buildSource)
    .replace(/^(\s*)model: .+$/gm, (_, indent: string) => `${indent}model: ${JSON.stringify(model.trim())}`);
  if (mode === "auto") source = source.replace("      Task: {{prompt}}", "      Goal or limits: {{prompt}}\n\n      Inspect the project and choose one focused, useful improvement that\n      fits this goal. You own the directive: do not ask the user to write it.\n      Put the chosen directive in summary, and the concrete implementation\n      plan and failure evidence in notes_for_next_agent. The builder will\n      execute your plan automatically. Avoid paid services and changes to\n      deployment, authentication, secrets, or security policy.");
  if (mode !== "ask" && testCommand.trim()) {
    source = source.replace(/    command: >\n(?:      .*\n)+/, () => `    command: ${JSON.stringify(testCommand.trim())}\n`);
  }
  // A code-reported field cannot be changed by an agent. Existing publish
  // holds keep these starter tasks local without changing worker behavior.
  source = source.replace(/^acceptance:/m, `  - name: keep-local\n    kind: code\n    reports_fields: true\n    command: "printf '%s\\n' '{\\\"jig_ui_delivery\\\":\\\"local\\\"}'"\nacceptance:`);
  source += '\npublish:\n  hold_when: "jig_ui_delivery != publish"\n';
  const hash = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(source))))
    .map((byte) => byte.toString(16).padStart(2, "0")).join("").slice(0, 16);
  return source.replace(/^name: .+$/m, `name: jig-task-${mode}-${hash}`);
}
