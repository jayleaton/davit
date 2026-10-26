# G5 prompt loop: real-model acceptance

The G5 features (`promptFeed`, `promptInterrupt`, `promptAttachments`, `promptOptions`, `promptTurnLease`,
`promptSteer`) are implemented and covered by hermetic tests: fake `claude`/`codex` scripts
(`internal/harness/testdata`), the in-process guest agent, the fake runtime. **None of it has run against a
real model yet.** The flags below come from `claude --help` (Claude Code 2.1.308) and `codex exec --help`
(codex-cli 0.163.0) and from the config keys those binaries accept. They are not proven until these steps
pass. Run them once API keys exist, on a real VM image with both harnesses installed (image capabilities
`harness.claude`, `harness.codex`).

## What each harness gets

| Control | Claude Code | Codex |
| --- | --- | --- |
| prompt | `-p --input-format stream-json --output-format stream-json --verbose`; one stream-json user line per message on stdin | `exec [resume] --json … -`, text on stdin |
| resume | `--resume SESSION` | `exec resume … SESSION -` |
| reasoningEffort | `--effort low\|medium\|high\|xhigh\|max` | `-c model_reasoning_effort="none\|minimal\|low\|medium\|high\|xhigh"` |
| fast | `--settings {"fastMode":true}` (settings key `fastMode`) | `-c service_tier="fast"` |
| images | base64 `image` content blocks in the user line | `--image PATH` (repeated, before `--json`) |
| other files | placed in the guest, named by path in the prompt | same |
| steer | `native`: another user line on the open stdin | `restart`: cancel, then `exec resume` with the message |
| turn timeout | guest exec deadline (SIGKILL to the process group) | same |
| credential grant | `ENVVAR=<placeholder>`, `ANTHROPIC_BASE_URL=<proxy>` | `ENVVAR=<placeholder>`, `OPENAI_BASE_URL=<proxy>/v1` |

## Setup

The proxy URL reaches the harness through `harness.Config.CredentialProxyURL`. The daemon sets it to
`credproxy.GuestBaseURL(<network config>)` whenever the proxy can listen (feature `credentialProxy` is
`available`) and leaves it empty otherwise; without it, grants are not injected.

1. Daemon config: `"harness": {"secretsFile": "/path/secrets.json"}` with `ANTHROPIC_API_KEY` and
   `OPENAI_API_KEY` (mode 0600). Alternatively pass `--env NAME` per prompt.
2. `davit new box --ttl 2h` on the real image. Record the host hardware, the image digest, and
   `davit exec box -- claude --version` / `codex --version`.
3. Below, `S` means `--secret ANTHROPIC_API_KEY` (claude) or `--secret OPENAI_API_KEY --harness codex`.

## Steps (run each for claude, then codex)

1. **Native resume.** `davit prompt box --new S "Remember the word heron. Reply OK."`, then
   `davit prompt box S "Which word did I ask you to remember?"` → answers heron.
   `davit --json conversations box` shows a `sessionId` and `turns: 2`.
2. **Feed, replay, reconnect.** `davit --json prompt-events box > all.jsonl` lists both turns' events in
   order, and the last line has `nextCursor`. Start a long turn with `--no-wait` (e.g. "run `for i in $(seq
   60); do echo $i; sleep 1; done` and summarize"). Run `davit prompt-events box --after <cursor from
   all.jsonl> --follow`, restart the daemon (`systemctl --user restart davit`) mid-turn, then run the same
   command again with the last printed cursor. Pass: no event is duplicated or missing (compare `promptId/seq`
   pairs), and the turn still ends `succeeded`.
3. **Interrupt.** Start two long turns in two conversations (`--new --no-wait`). Then
   `davit interrupt box --conversation <first>` cancels only the first, within 10s.
   `davit interrupt box --all` cancels the other. A repeat prints `no active turn`. Each cancelled turn is
   `cancelled` with error code `cancelled`, and the next prompt in that conversation resumes its session.
4. **Attachments.** Make a PNG with one red square. `davit prompt box --new S --attach square.png "What
   colour is the square?"` → red. Copy a PDF into the guest (`davit cp spec.pdf box:/home/<user>/spec.pdf`),
   then `--attach-guest /home/<user>/spec.pdf "Summarize the attached spec in one line"` → a summary. Check
   that `/tmp/davit-<promptId>/square.png` exists in the guest.
5. **Options.** `davit prompt box --new S --effort high --fast "Say hi"` succeeds. Then confirm that the
   harness applied the options:
   - Codex: the session rollout (`~/.codex/sessions/**/rollout-*.jsonl` in the guest) records effort
     `high` and service tier `fast`.
   - Claude: the `system`/`init` raw event or the session transcript (`~/.claude/projects/**.jsonl`) shows the
     effort and fast mode.

   If an account or model rejects fast mode or an effort level, the turn must fail with `harness_failed` and
   the harness's message, never succeed silently. Record which models accept which values.
6. **Turn lease and timeout.** `davit new lease --ttl 2m`. Start a turn that takes about 4 minutes with
   `--turn-timeout 10m --no-wait`. At 2m the VM must still be running (it is held by the turn). Within about
   15s after the turn ends, the VM auto-stops. Repeat on a fresh VM with `--turn-timeout 30s` on a turn that
   takes longer. The turn fails with `turn_timeout` at about 30s, and the VM then stops at its TTL.
7. **Steer.**
   - Claude: start a long turn (`--no-wait`). While it runs, `davit steer box <prompt> "Also end with the
     word DONE."` → `prompt.steered` with `delivery: native` and the same prompt ID. The turn then ends
     `succeeded` (the process exits), and the output ends with DONE. **Known risk:** the service closes stdin
     once it has counted as many `result` lines as messages it wrote. If Claude Code folds a mid-turn message
     into the running turn and emits only one `result`, the turn stays open until its deadline. In that case,
     change `Service.endInput` (internal/harness/control.go) to close stdin after the first result that
     follows the last write, and record the observed behaviour here.
   - Codex: same steps. The steer reports `delivery: restart`, the old turn is `cancelled` ("cancelled to
     restart…"), and the new turn resumes the same `sessionId` with the message.
   - Restart the daemon during a Claude turn, then steer it: expect `delivery: restart` (stdin did not survive
     the restart).
8. **Usage.** A steered Claude turn reports summed tokens. Check that `costUsd` matches the total Claude Code
   reports (the service treats `total_cost_usd` as a running total and takes the maximum).

9. **Credential grants (with G4).** Instead of `--secret`, create a profile and grant it to the VM
   (`davit credentials …`). A plain `davit prompt box --new "Say hi"` must work for both harnesses. In
   the guest, the harness's environment has only the placeholder (`davit exec box -- cat
   /proc/<harness pid>/environ`), plus `ANTHROPIC_BASE_URL=http://172.16.0.1:15080` (Claude) or
   `OPENAI_BASE_URL=http://172.16.0.1:15080/v1` (Codex). Passing `--env ANTHROPIC_API_KEY` as well must
   still use the grant, and the turn's first event must be the raw `credential_env_conflict` notice. After
   `davit credentials revoke`, the next turn must fail with the provider's 401/403 from the proxy, not
   succeed. Check that Codex honours `OPENAI_BASE_URL` for its built-in OpenAI provider. If it does not, its
   config needs `-c model_providers…` and the adapter must pass that.

Record the results (pass/fail per step and harness, model names, hardware, sample counts) in this file.

## G14 harnesses: OpenCode and pi

Both adapters are built from their CLIs' documentation and source, not from runs: OpenCode from
`packages/opencode/src/cli/cmd/run.ts` at tag v1.19.3 (anomalyco/opencode), pi from the docs in the
`@earendil-works/pi-coding-agent` 1.4.0 package (`docs/cli.md`, `docs/json.md`) and earendil-works/pi
source. Hermetic tests use `internal/harness/testdata/fake-opencode` and `fake-pi`. Until the steps below
pass, both report `experimental: true` in `capabilities.harnesses`. Once a harness passes, clear
`Experimental` in its adapter and record the results here.

| Control | OpenCode | pi |
| --- | --- | --- |
| prompt | `run --format json --auto`, text on stdin (piped stdin is the message) | `--mode json`, text on stdin (prepended to an empty first prompt) |
| events | `step_start`, `text`, `tool_use` (call and result in one line, once finished), `step_finish` (per-step usage, summed), `error`; no deltas, `sessionID` on every line | `session` header (`id`), `message_end` (authoritative assistant text, usage, `stopReason`), `tool_execution_start`/`_end`, `auto_retry_start`; deltas stay raw |
| failure | `error` line, exit 1 | `stopReason` `error`/`aborted` with `errorMessage`; **exit 0** |
| resume | `--session SESSION` | `--session-id SESSION` (exact ID in the conversation's cwd) |
| reasoningEffort | `--variant` (per provider: Anthropic `high\|max`, OpenAI `none`…`xhigh`, Google `low\|high`) | `--thinking off\|minimal\|low\|medium\|high\|xhigh\|max` (clamped to the model) |
| fast | none | none |
| images | `--file PATH` (OpenCode's Read tool attaches it) | `@PATH` argument |
| steer | `restart` | `restart` (pi's RPC mode can steer natively; not used yet) |
| approvals | `--auto` approves every request | pi has none; trust-gated project files are skipped headless |
| credential grant | placeholder plus `OPENCODE_CONFIG_CONTENT={"provider":{"anthropic":{"options":{"baseURL":"<proxy>/v1"}}}}` (OpenAI: `<proxy>/v1`) | **not applied**: pi reads no base URL variable, so the turn gets the `credential_grants_unsupported` notice and the caller's env |

Setup as above, on images built from `images/manifests/profiles/ubuntu-24.04-opencode.json` and
`ubuntu-24.04-pi.json` (capabilities `harness.opencode`, `harness.pi`). Record
`davit exec box -- opencode --version` and `pi --version`. `S` means `--harness opencode --secret
ANTHROPIC_API_KEY --model anthropic/<model>` or `--harness pi --secret ANTHROPIC_API_KEY --model
anthropic/<model>`.

1. **Native resume.** As step 1 above (heron). `conversations` shows OpenCode's `ses_…` or pi's UUID as
   `sessionId`, and turn 2's argv (`davit exec box -- ps -o args -C opencode,node`) carries `--session`
   or `--session-id` with it.
2. **Events.** A turn that runs a shell command yields `conversation.started`, `tool.call`, `tool.result`
   and `message.delta` in order, and `usage` is the sum the CLI reports (OpenCode: the `step_finish`
   tokens; pi: each assistant `message_end`'s `usage`). Compare one turn's raw lines with the normalized
   events.
3. **Errors.** With a wrong key (`--env ANTHROPIC_API_KEY=sk-bad`), the turn fails with `harness_failed`
   and the provider's message. For pi the process exits 0, so only the stop reason can fail the turn.
   Check that it does, and that a retried transient error (`auto_retry_start`) that then succeeds ends
   `succeeded`.
4. **Interrupt.** As step 3 above. A cancelled turn ends `cancelled`, and the next prompt resumes the
   session. OpenCode has a SIGINT path and pi a SIGTERM path; the guest kills the process group, so check
   that no `opencode`/`pi` process is left behind (`davit exec box -- pgrep -a -f 'opencode|pi --mode'`).
5. **Attachments.** As step 4 above. The red-square PNG must reach the model as an image: OpenCode
   through `--file` (the Read tool's "Image read successfully" result in the raw events), pi through
   `@path`. If OpenCode answers from the file name rather than the pixels, move images to inline data and
   record why.
6. **Options.** `--effort high` for each. OpenCode: `--variant high` applies only where the provider
   defines it. Record what an undefined variant does (for example `--effort minimal` with an Anthropic
   model): a silent ignore must be listed here. pi: `--effort max` on a model without it is clamped; the
   raw `thinking_level_changed` event or the session file shows the level. `--fast` must be refused
   (`unsupported_capability`).
7. **Steer.** Start a long turn, steer it: `delivery: restart`, the old turn is `cancelled`, and the new
   turn resumes the same session with the message.
8. **Credential grants (with G4).** OpenCode: with an Anthropic grant and no `--secret`, a plain prompt
   works through the proxy. The harness environment has the placeholder and `OPENCODE_CONFIG_CONTENT` with
   the proxy `baseURL`, and the proxy log shows the request. Repeat with an OpenAI grant and an
   `openai/<model>`. pi: the turn's first event is the `credential_grants_unsupported` notice; with
   `--secret ANTHROPIC_API_KEY` the turn still works. Before pi can use grants, it needs a base URL
   override, for example a baked-in extension that calls `pi.registerProvider("anthropic", {baseUrl})`.
9. **Session cwd.** Create a conversation with `--cwd /work/a`, then another with `--cwd /work/b` that
   resumes nothing. pi must never stop at its cross-project "fork this session?" question (`--session-id`
   is exact and per project).
