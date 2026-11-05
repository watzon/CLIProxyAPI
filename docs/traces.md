# Request traces

Traces record what your coding agents do through the proxy. You see tokens, API-equivalent cost and active time per project. You also see each conversation: messages, tool calls, diffs, errors and retries.

Traces are off by default.

## What is recorded

For every model request, the proxy stores one turn. A turn has:

- The client, the model, the account that served it, the status and the timing.
- Token counts and cost, with cache reads and writes counted apart.
- The tool calls and the file changes they made.

Turns group into sessions. A session is one client conversation. A subagent has its own session under the session of its parent.

The `capture` setting controls the content:

- `metadata` stores everything above, plus the folder and the title of each session.
- `full` also stores the message content: prompts, replies, tool calls and tool results.

Known secret patterns (API keys, tokens, private keys) are replaced before content is stored. Set `redact-secrets: false` to turn that off.

## Enable traces

Add this to `config.yaml`:

```yaml
traces:
  enabled: true
  capture: metadata
```

Restart the proxy once. After that, every key except `dir` applies on hot reload. The full list of keys, with defaults, is in the `traces` block of `config.example.yaml`.

The Management API serves the data under `/v8/management/traces`. Recording never blocks or fails a client request.

## How the project of a session is chosen

The proxy picks the project of a session from the first signal that exists in this list:

| Order | Signal | `project_source` |
| --- | --- | --- |
| 1 | The `X-CPA-Project` header. | `header` |
| 2 | The project in the base URL: `/p/<project>`. | `path` |
| 3 | A rule in `traces.projects.rules` that matches the session folder. | `rule` |
| 4 | A git remote: the `X-CPA-Git-Remote` header, the remote that Codex sends, or a remote that the proxy saw earlier for the same folder. | `remote` |
| 5 | The name of the session folder. | `folder` |
| 6 | The parent session, or an earlier turn of the same session. | `inherited` |
| 7 | Nothing. The project is `unassigned`. | `none` |

The sessions API shows the chosen source as `project_source` on each session. The value is one of `header`, `path`, `rule`, `remote`, `folder`, `inherited` or `none`.

### Where the folder comes from

The proxy reads the folder from the request, in a way that fits each client:

- Claude Code: the `Primary working directory:` line in the system prompt.
- Claude Code through its SDK: the system prompt has no folder. The folder comes from the user messages instead. The proxy takes the first `Primary working directory:` line it finds, in the `SessionStart hook additional context` text or in a `# Environment update` text. If that line ends with `(was X)`, the folder is `X`, the folder before the shell moved.
- Codex: the `X-Codex-Turn-Metadata` header, or the `<cwd>` element in the context text.
- OpenCode and OMP: the working directory in the prompt.

The `X-CPA-Cwd` header always replaces the folder that the proxy reads from the request.

### Worktree folders do not name a project

A folder inside a worktree container, like a scratch folder or a git worktree, has a random name. That name is a poor project name. The proxy keeps such a folder as the session folder. It does not use the folder name as the project. If no earlier signal in the list applies, the session is `unassigned`.

The list of containers is `traces.projects.worktree-dirs`. Each entry is a folder pattern. Home folders are written as `~`. A `*` matches one folder name. A pattern that starts with `*/` matches at any depth. A folder below the container matches too. The default list is:

```yaml
traces:
  projects:
    worktree-dirs:
      - "~/.t3/scratch/*"
      - "~/.claude/worktrees/*"
      - "~/.codex/worktrees/*"
      - "*/.worktrees/*"
      - "*/.claude/worktrees/*"
```

Set `worktree-dirs: []` to turn the check off. A rule in `traces.projects.rules` that matches a worktree folder still wins.

### Rules

A rule maps a folder pattern to a project name. The first matching rule wins. `{name}` captures one folder name.

```yaml
traces:
  projects:
    rules:
      - match: "~/code/{name}"
        project: "{name}"
```

### Stored sessions are corrected

When the proxy starts for the first time after an update, and after every change of `rules` or `worktree-dirs`, a background job runs. It does not delay startup or requests. The job:

1. Finds sessions that have no folder, or a title that is a compaction summary (`This session is being continued from a previous conversation...`).
2. Reads the stored request content of their turns and derives the folder and the title again. This works only for turns that were captured with `capture: full`. Other sessions are skipped.
3. Resolves the project of every session again with the order above.

Titles never come from compaction summaries or from injected context (`<system-reminder>` blocks, `SessionStart hook additional context`, `# Environment update`). The title is the first line of the first real prompt, cut to 120 characters.

## Client setup

Most clients need no setup. The proxy finds the folder by itself. Use the hints below when a client hides the folder, or when you want a project name that does not depend on the folder.

### Headers

| Header | Value | Effect |
| --- | --- | --- |
| `X-CPA-Project` | A project name. | Sets the project. Wins over everything else. |
| `X-CPA-Cwd` | An absolute folder. | Replaces the folder that the proxy reads from the request. |
| `X-CPA-Git-Remote` | A git remote URL. | Sets the git remote. SSH and HTTPS forms of the same repository are equal. |
| `X-CPA-Client` | A label. | Replaces the client name, for example `t3code`. |

The proxy reads these headers and removes them. It removes every request header that starts with `X-CPA-`, known or not, so none of them reaches an upstream provider. This also holds when traces are off, and for the WebSocket handshake that Codex uses.

### Project in the base URL

A client that cannot send headers can name the project in its base URL. Put `/p/<project>` in front of the usual path:

```text
https://proxy.example.com:8317/p/my-app
https://proxy.example.com:8317/p/my-app/v1
https://proxy.example.com:8317/p/my-app/backend-api
```

The proxy removes the `/p/<project>` prefix before it routes the request. Every API route works as it does without the prefix, for HTTP and for WebSocket upgrades. Percent escapes in the project name are decoded. An empty project name returns `400`. The prefix does not apply to the Management API or the control panel.

The prefix works when traces are off too. The project is then not used.

### Claude Code

Claude Code reads `ANTHROPIC_CUSTOM_HEADERS`. Put one `Name: value` pair on each line:

```sh
export ANTHROPIC_BASE_URL="https://proxy.example.com:8317"
export ANTHROPIC_CUSTOM_HEADERS="X-CPA-Project: my-app
X-CPA-Git-Remote: git@github.com:acme/my-app.git"
claude
```

Or use the base URL instead of the header:

```sh
export ANTHROPIC_BASE_URL="https://proxy.example.com:8317/p/my-app"
```

### Codex

Codex can send HTTP headers from a model provider in `config.toml`. Use `http_headers` for fixed values. Use `env_http_headers` to read a value from an environment variable. Codex skips the header when the variable is not set.

```toml
[model_providers.cpa]
name = "CLIProxyAPI"
base_url = "https://proxy.example.com:8317/v1"
env_key = "CPA_API_KEY"
wire_api = "responses"

[model_providers.cpa.env_http_headers]
"X-CPA-Project" = "CPA_PROJECT"
"X-CPA-Cwd" = "CPA_CWD"
"X-CPA-Git-Remote" = "CPA_GIT_REMOTE"
```

Codex sends these headers on its WebSocket connection too.

### A wrapper that finds the main repository

A git worktree has its own folder, but it belongs to one main repository. A wrapper script can find that repository and send its folder and remote. The main repository is the parent of the git common folder:

```sh
#!/bin/sh
# Usage: cpa-run <command> [args...]
common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
if [ -n "$common" ]; then
  CPA_CWD="$(dirname "$common")"
  CPA_GIT_REMOTE="$(git remote get-url origin 2>/dev/null)"
  export CPA_CWD CPA_GIT_REMOTE
  export ANTHROPIC_CUSTOM_HEADERS="X-CPA-Cwd: $CPA_CWD
X-CPA-Git-Remote: $CPA_GIT_REMOTE"
fi
exec "$@"
```

Run `cpa-run claude` or `cpa-run codex`. The Codex provider above reads `CPA_CWD` and `CPA_GIT_REMOTE`. A bare repository has no working tree, so `dirname` of its common folder is not a project folder. Do not use the wrapper there.
