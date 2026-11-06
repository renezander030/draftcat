# Agent harness hooks

Claude Code, Codex, GitHub Copilot CLI and Gemini CLI can run a command before
every tool call and block the call on its answer (`PreToolUse`, or
`BeforeTool` in Gemini CLI). `draftcat gate hook` is that command: it puts each
call the harness is about to make through the [tool-call gate](tool-gate.md),
and the call runs only when the gate allowed it and released its one-time
permit. `draftcat gate install` writes the hook into the harness's own settings
file.

```bash
draftcat gate install claude-code        # .claude/settings.json in this project
draftcat gate install codex              # .codex/hooks.json
draftcat gate install copilot-cli        # .github/hooks/draftcat.json
draftcat gate install gemini-cli         # .gemini/settings.json
draftcat gate install claude-code --user # ~/.claude/settings.json instead
```

## What happens on a tool call

1. The harness runs `draftcat gate hook --format <harness>` with the call on
   standard input: the tool name, its input, the session id and, where the
   harness has one, the tool-use id.
2. The hook asks the gate (`POST /gate/tool-call`) with
   - `tool`: the harness's tool name, such as `Bash`, `Write` or
     `mcp__github__create_issue`;
   - `args`: the tool input exactly as the harness sent it (numbers keep their
     spelling, so the hash the human approves is the hash of this input);
   - `agent`: `--agent`, by default `claude-code`, `codex`, `copilot-cli` or
     `gemini-cli`;
   - `run_id`: the harness's session id;
   - `action_id`: derived from the session id and the tool-use id, so a hook
     the harness runs again for the same call joins the decision already made
     instead of asking twice. Copilot CLI and Gemini CLI send no tool-use id;
     there the session id, the payload's timestamp, the working directory, the
     tool and its arguments identify the call.
3. A decision that needs a human is held, then polled
   (`GET /gate/tool-call/<id>?wait=`), for up to `--wait` (default `5m`).
4. An allowed decision is consumed (`POST /gate/tool-call/<id>/consume`).
   Only a consume that answers `permit: execute` lets the call run.
5. The hook answers in the harness's format: allow, or deny with the gate's
   reason, which the harness hands back to the agent.

## Installing

`draftcat gate install <harness>` upserts one entry in the harness's settings
file. Every other key and hook in the file stays, in its place; a second run
changes nothing; a run with other options replaces draftcat's entry where it
is; `--remove` takes out only draftcat's entry. The file is replaced
atomically, and a symlinked settings file stays a symlink. `--dry-run` prints
the resulting file without writing it.

| Harness | Project file (`--dir`, default `.`) | `--user` file | Event | Default matcher |
|---|---|---|---|---|
| `claude-code` | `.claude/settings.json` | `~/.claude/settings.json` | `PreToolUse` | `^(Bash\|Write\|Edit\|MultiEdit\|NotebookEdit\|mcp__.*)$` |
| `codex` | `.codex/hooks.json` | `$CODEX_HOME/hooks.json` (`~/.codex`) | `PreToolUse` | `^(Bash\|apply_patch\|mcp__.*)$` |
| `copilot-cli` | `.github/hooks/draftcat.json` | `$COPILOT_HOME/hooks/draftcat.json` (`~/.copilot`) | `PreToolUse` | `Bash\|Write\|Edit` |
| `gemini-cli` | `.gemini/settings.json` | `~/.gemini/settings.json` | `BeforeTool` | `^(run_shell_command\|write_file\|replace\|mcp_.*)$` |

The default matchers send the shell, the file-writing tools and MCP tools to
the gate, and leave read-only tools (reading, searching, listing) to the
harness. `--matcher` replaces the default in the harness's own matcher syntax.
Copilot CLI names an MCP tool `<server>-<tool>` with no common prefix, so add
the MCP tools to gate there by name: `--matcher 'Bash|Write|Edit|github-create_issue'`.

The install prints which tool names its matcher sends:

```
added the draftcat PreToolUse hook in /work/repo/.claude/settings.json (matcher ^(Bash|Write|Edit|MultiEdit|NotebookEdit|mcp__.*)$)
tool names the hook sends to the gate: Bash, Write, Edit, MultiEdit, NotebookEdit, mcp__<server>__<tool>
the gate denies every tool not in tool_gate.tools: add a rule for each name it should decide
```

The installed command runs `draftcat` from `PATH` (`--bin` names another
executable) and passes `--agent`, `--token-env` and `--wait` when given. It
never contains a token or a URL: the hook reads them from the environment the
harness runs in.

| Variable | Meaning |
|---|---|
| `DRAFTCAT_GATE_URL` | the gate, default `http://127.0.0.1:8088` (`--url` overrides) |
| `DRAFTCAT_GATE_TOKEN` | the bearer token; falls back to `DRAFTCAT_WEBHOOK_SECRET`. `--token-env VAR` reads another variable. |

The harness's own timeout for the hook is set a minute past `--wait` (6
minutes by default), so the hook answers before the harness gives up on it.
Codex runs a new or changed project hook only after you review and trust it.

## Configuring the gate

The gate decides only tools it lists: a name the hook sends that is not in
`tool_gate.tools` is denied. A setup for Claude Code that lets read-only git
and `ls` commands through, sends every other shell command and every write
outside `docs/` to a human, and always asks before an issue is opened:

```yaml
webhook:
  enabled: true
  secret_env: DRAFTCAT_WEBHOOK_SECRET
  max_body_bytes: 1048576            # a Write call carries the whole file

tool_gate:
  enabled: true
  callers:
    - name: claude-code              # the hook's --agent, default claude-code
      token_env: GATE_TOKEN_CLAUDE   # export DRAFTCAT_GATE_TOKEN with this value for the harness
      tools: [Bash, Write, Edit, mcp__github__create_issue]
  tools:
    - name: Bash
      args:
        # Plain words only: a `;`, `|`, `&`, `$`, quote or redirect is a mismatch.
        command: {regex: '^(git (status|diff|log)|ls)( [-A-Za-z0-9_./=]+)*$'}
      on_mismatch: approve           # any other command: a human decides
    - name: Write
      args:
        file_path: {glob: "/work/repo/docs/*"}
      on_mismatch: approve
    - name: Edit
      args:
        file_path: {glob: "/work/repo/docs/*"}
      on_mismatch: approve
    - name: mcp__github__create_issue
      require_approval: true
```

Claude Code sends `Bash` as `{"command", "description", "timeout"}`, `Write`
as `{"file_path", "content"}` and `Edit` as `{"file_path", "old_string",
"new_string"}`; a rule constrains only the arguments it names.
`draftcat gate check Bash --args '{"command":"ls -la"}'` shows what the rule
decides without running anything.

With a caller token, the hook's agent name must be the caller's name: install
with `--agent` set to it when the caller is not called `claude-code`, `codex`,
`copilot-cli` or `gemini-cli`.

## How a decision reaches the operator

A call that needs a human is an ordinary tool-gate ask: the operator channel
(Telegram or a relay) shows the agent, the tool, its arguments and the rule
that sent it there, `/pending` and `draftcat pending` list it as
`tool-gate / <tool>`, and the decision and the consume each leave a receipt.
Refusals the gate makes on its own (an unlisted tool, an argument outside a
`deny` rule, the repeat guard) reach the operator as notices, as described in
[Telling the operator](tool-gate.md#telling-the-operator).

The hook waits `--wait` for the decision. When the human has not decided by
then, the hook withdraws the ask (the prompt closes and its permit can never be
used) and blocks the call. The agent sees the reason and can try again; inside
`tool_gate.repeat_window` the gate remembers that identical call as denied, as
it does for any caller that hangs up.

## Fail-closed behaviour

| Situation | The harness sees |
|---|---|
| allowed and the permit consumed (`permit: execute`) | allow |
| denied by a rule, the operator, the repeat guard or a freeze | deny, with the gate's reason and rule |
| no decision within `--wait` | deny; the ask is withdrawn |
| the consume is refused (permit expired, already used, frozen, policy changed) | deny |
| the gate unreachable, an HTTP error, an answer without a decision | deny |
| token missing, token or signature refused (`401`), caller and agent differ (`403`) | deny |
| tool input above `webhook.max_body_bytes` (`413`) | deny |
| a payload the hook cannot read, or a hook fired for another event | deny |
| the hook cannot run at all (draftcat missing from `PATH`) | blocked: the installed command ends in `\|\| { ...; exit 2; }` for Claude Code, Codex and Gemini CLI; Copilot CLI denies a hook that exits non-zero |

Every block is answered in the harness's format with the reason, and the
reason is also written as one line to standard error. A usage error in the
hook's own flags exits 2, which every one of these harnesses treats as a
block.

How the answer is written:

| Harness | allow | deny |
|---|---|---|
| Claude Code | `hookSpecificOutput.permissionDecision: allow` | `permissionDecision: deny` with `permissionDecisionReason` |
| Codex | no output (Codex refuses an explicit allow without rewritten input) | `hookSpecificOutput.permissionDecision: deny` with the reason |
| Copilot CLI | `permissionDecision: allow`, at the top level and in `hookSpecificOutput` | `permissionDecision: deny` with the reason, both places |
| Gemini CLI | `{"decision": "allow"}` | `{"decision": "deny", "reason": ...}` |

An allow skips the harness's own permission prompt for that call in Claude
Code and Copilot CLI; Claude Code's deny and ask rules still apply on top of
it.

Other properties:

- **One call, one execution.** A permit is consumed once. A hook run a second
  time for a call whose permit is already used is blocked, and so is the
  second of two draftcat hooks for the same call. Install the hook in one
  scope (the project or `--user`), not both.
- **Signed requests.** Every request body the hook sends is signed with its
  token (`X-Draftcat-Signature`), so the hook works with
  `webhook.require_signature: true`. The gate's clock and the harness's clock
  must agree within `webhook.max_skew_seconds`.
- **Harness timeouts.** Most of these harnesses let a call through when a hook
  times out (Copilot CLI always does), which is why the install sets the
  harness's timeout past `--wait`. Re-run the install with the new `--wait`
  rather than raising it in the command alone.
- **Body size.** The gate reads at most `webhook.max_body_bytes` (64 KiB by
  default) and 1 MiB per ask. A `Write` of a larger file is denied; raise the
  limit if that is not what you want.

## The boundary

The hook is a control inside a harness the operator runs. It holds as long as
the harness runs its hooks, and no longer:

- An agent that can write files can try to edit the settings file that holds
  the hook. Gate `Write` and `Edit` on those paths, prefer the `--user` (or
  the harness's managed) settings, and keep the hook's file under review.
- Claude Code's `disableAllHooks`, Codex's `[features] hooks = false`, a
  harness started without hooks, or a tool the matcher does not name all skip
  the gate.
- The hook sees what the harness reports. A shell command does whatever its
  string says; a rule on `command` is only as strict as its regex.
- The gate token is in the agent's environment. It lets its holder ask and
  consume, never approve; a caller token with `tools:` limits what it can ask
  about.

Where an action must not run unapproved, do not rely on the hook alone. Keep
the credential with the tool, out of the agent's environment, and have the
tool check an execution grant before it acts:

```bash
draftcat gate verify-grant "$GRANT" --tool send_email --args "$ARGS" --max-age 5m
```

A harness hook cannot hand the grant to the tool: the consume answer goes back
to the harness, not to the tool. A tool that checks grants therefore asks the
gate and consumes the permit itself (see [Execution
grants](tool-gate.md#execution-grants)); the hook stays the harness-side layer
in front of it.

## Limits

- The hook does not send the tool's definition, so a rule with
  `definition_sha256` treats every call through the hook as a mismatch.
- The hook does not report what the tool did (`/complete`); the gate records
  the consume, and `execution_status` stays `unreported`.
- Tool names and argument shapes are the harness's own and change with its
  versions. A denial notice names the tool the gate saw, which is the name to
  put in `tool_gate.tools`.

## Reference

```
draftcat gate hook --format claude|codex|copilot|gemini [--url URL] [--token-env VAR] [--agent NAME] [--wait 5m]
draftcat gate install claude-code|codex|copilot-cli|gemini-cli [--matcher REGEX] [--dir DIR | --user]
                      [--agent NAME] [--token-env VAR] [--wait 5m] [--bin PATH] [--dry-run] [--remove]
```
