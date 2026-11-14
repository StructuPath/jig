// startHelp.ts — what to do when the control plane refuses to start a job.
// Admission answers with a code and git's own words ("cannot resolve HEAD",
// "does not appear to be a git repository"), which is exact and useless to
// someone starting a new project: the fix is two commands they have not been
// shown. This maps each refusal the control plane actually returns to the
// plain cause and those commands. The raw error stays on the page beside it.
import { APIError } from "./api";

export interface StartHelp {
  title: string;
  detail: string;
  commands: string[];
}

function shellQuote(value: string): string {
  return /^[\w@%+=:,./~-]+$/.test(value) ? value : `'${value.replace(/'/g, `'\\''`)}'`;
}

export function startHelp(error: Error | null, repository: string): StartHelp | null {
  if (!(error instanceof APIError)) return null;
  const project = repository.trim();
  const github = /^(https:\/\/)?github\.com\/([^/\s]+\/[^/\s]+?)(\.git)?\/?$/.exec(project);

  if (error.code === "invalid_target") {
    if (error.message.includes("no such file or directory")) {
      return {
        title: "That folder does not exist",
        detail: "A job starts from a commit in a Git project. Create the folder with a first commit, then start again.",
        commands: [`mkdir -p ${shellQuote(project)} && cd ${shellQuote(project)}`, "git init -b main", 'echo "# New project" > README.md', 'git add README.md && git commit -m "Initial commit"'],
      };
    }
    return {
      title: "That is not a project jig can find",
      detail: "Use the full path of a folder on this Mac, such as /Users/you/dev/my-project, or a GitHub name, such as github.com/owner/repo.",
      commands: [],
    };
  }

  if (error.code !== "unresolvable_ref") return null;

  if (github) {
    const folder = shellQuote(github[2].split("/")[1]);
    return {
      title: "GitHub has no repository with that name, or it has no commit yet",
      detail: "Create the repository and push a first commit, then start again. A private repository also needs the gh login on this Mac to have access to it.",
      commands: [`mkdir -p ${folder} && cd ${folder}`, "git init -b main", 'echo "# New project" > README.md', 'git add README.md && git commit -m "Initial commit"', `gh repo create ${shellQuote(github[2])} --private --source . --push`],
    };
  }
  if (error.message.includes("does not appear to be a git repository")) {
    return {
      title: "This folder is not a Git project yet",
      detail: "A job starts from a commit, so the folder needs Git and one commit. A blank folder also needs one file to commit.",
      commands: [`cd ${shellQuote(project)}`, "git init -b main", 'echo "# New project" > README.md', 'git add -A && git commit -m "Initial commit"'],
    };
  }
  return {
    title: "This Git project has no commit yet",
    detail: "A job starts from a commit. Commit at least one file, then start again.",
    commands: [`cd ${shellQuote(project)}`, 'echo "# New project" > README.md', 'git add -A && git commit -m "Initial commit"'],
  };
}

/**
 * workerCommand is the command that starts a worker for a runtime. `jig
 * worker` runs Claude Code unless told otherwise, so a job that needs another
 * runtime — Codex for the task form's own jobs — names it, or it stays queued.
 */
export function workerCommand(runtime?: string): string {
  return runtime ? `jig worker --runtime ${runtime}` : "jig worker";
}
