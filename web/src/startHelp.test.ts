import { describe, expect, it } from "vitest";
import { APIError } from "./api";
import { startHelp } from "./startHelp";

// Each message is the control plane's real answer for that target.
const refused = (code: string, message: string) => new APIError(code, message, 400);

describe("startHelp", () => {
  it("explains a blank folder and gives the commands that fix it", () => {
    const help = startHelp(refused("unresolvable_ref",
      `cannot resolve "HEAD" in "file:///Users/v/dev/new": fatal: '/Users/v/dev/new' does not appear to be a git repository\nfatal: Could not read from remote repository.`),
      "/Users/v/dev/new");
    expect(help?.title).toBe("This folder is not a Git project yet");
    expect(help?.commands).toEqual(["cd /Users/v/dev/new", "git init -b main", 'echo "# New project" > README.md', 'git add -A && git commit -m "Initial commit"']);
  });

  it("tells a repository with no commit apart from a folder with no Git", () => {
    const help = startHelp(refused("unresolvable_ref", 'cannot resolve "HEAD" in "file:///Users/v/dev/new": '), "/Users/v/dev/new");
    expect(help?.title).toBe("This Git project has no commit yet");
    expect(help?.commands).not.toContain("git init -b main");
  });

  it("gives the gh command for a GitHub name that does not resolve", () => {
    const help = startHelp(refused("unresolvable_ref",
      `cannot resolve "HEAD" in "github.com/StructuPath/new-app": remote: Repository not found.`), " github.com/StructuPath/new-app ");
    expect(help?.title).toBe("GitHub has no repository with that name, or it has no commit yet");
    expect(help?.commands.at(-1)).toBe("gh repo create StructuPath/new-app --private --source . --push");
  });

  it("quotes a GitHub name that carries shell characters", () => {
    const help = startHelp(refused("unresolvable_ref",
      `cannot resolve "HEAD" in "github.com/owner/app;touch-x": remote: Repository not found.`), "github.com/owner/app;touch-x");
    expect(help?.commands[0]).toBe("mkdir -p 'app;touch-x' && cd 'app;touch-x'");
    expect(help?.commands.at(-1)).toBe("gh repo create 'owner/app;touch-x' --private --source . --push");
  });

  it("explains a folder that does not exist, quoting a path with spaces", () => {
    const help = startHelp(refused("invalid_target",
      `target repository "/Users/v/my app": canonicalize repository path: lstat /Users/v/my app: no such file or directory`), "/Users/v/my app");
    expect(help?.title).toBe("That folder does not exist");
    expect(help?.commands[0]).toBe("mkdir -p '/Users/v/my app' && cd '/Users/v/my app'");
  });

  it("explains a name that is neither a path nor a GitHub repository", () => {
    const help = startHelp(refused("invalid_target",
      `target repository "my new project": repository identity "my new project" is not a recognized form`), "my new project");
    expect(help?.title).toBe("That is not a project jig can find");
    expect(help?.commands).toEqual([]);
  });

  it("says nothing for a refusal it does not know, or an error that is not the control plane's", () => {
    expect(startHelp(refused("definition_not_found", "no such definition"), "/x")).toBeNull();
    expect(startHelp(new Error("network down"), "/x")).toBeNull();
    expect(startHelp(null, "/x")).toBeNull();
  });
});
