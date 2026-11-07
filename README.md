# Roast

Roast is an adversarial, cross-model code reviewer. A finder model proposes
defects in a Git diff, each with a code quote and a concrete fix; an
independent verifier model then re-reads the cited code and confirms or refutes
every claim before it reaches the report. The two roles can use different
providers, endpoints, and wire protocols.

Roast has no telemetry. Models see only what Roast sends them, and in
interactive mode they can navigate the local checkout only through bounded,
read-only file, grep, and listing tools.

## Install

With Homebrew:

```sh
brew install --cask bluesky-social/tap/roast
```

Homebrew 7 and later only load casks from third-party taps you trust, though
naming the cask in full (as above) counts. For a plain `brew upgrade` to update Roast,
also run `brew trust --tap bluesky-social/tap`.

Or download an archive for macOS, Linux, or Windows (amd64 and arm64) from
[GitHub Releases](https://github.com/bluesky-social/roast/releases) and verify
it against the published SHA-256 checksums. Or, with Go 1.27.3 or newer:

```sh
go install github.com/bluesky-social/roast/cmd/roast@latest
```

On Arch Linux, community-maintained (unofficial) packages are in the
[AUR](https://aur.archlinux.org/packages/roast):

```sh
yay -S roast      # latest release
yay -S roast-git  # built from main
```

To build from a checkout, run `go build ./cmd/roast`. `roast --version` prints
the version, commit, and build date.

Builds that report a version other than `dev` run a cached, informational check
for newer stable releases. It never blocks a command or changes its exit
status, and it fails open; set `ROAST_NO_UPDATE_CHECK=1` to turn it off.

Upgrading from a pre-1.1 configuration? See
[Upgrading from pre-1.1 configurations](docs/upgrading.md).

### Agent Skill

```sh
roast skill install
roast skill status
```

This installs the bundled skill to `~/.agents/skills/roast`, and also to
`$CLAUDE_CONFIG_DIR/skills/roast` (default `~/.claude/skills/roast`) when Claude
Code is detected and to `$XDG_CONFIG_HOME/polytoken/skills/roast` (default
`~/.config/polytoken/skills/roast`) when Polytoken is detected. `--dir` limits
either command to one parent skills directory.

## Quick start

```sh
export OPENAI_API_KEY='...'
export ROAST_FINDER_MODEL='your-finder-model'
export ROAST_VERIFIER_MODEL='your-verifier-model'
roast doctor
roast review --task 'add retry handling'
```

Both roles default to the OpenAI Responses protocol at
`https://api.openai.com/v1`.

## Configure

### Environment variables

Each role can use its own endpoint, credential, and protocol:

```sh
export ROAST_FINDER_BASE_URL='https://finder.example/v1'
export ROAST_FINDER_API_KEY='...'
export ROAST_FINDER_MODEL='finder-model'
export ROAST_FINDER_PROTOCOL='chat'

export ROAST_VERIFIER_BASE_URL='https://verifier.example/v1'
export ROAST_VERIFIER_API_KEY='...'
export ROAST_VERIFIER_MODEL='verifier-model'
export ROAST_VERIFIER_PROTOCOL='responses'
```

API-key protocols are `chat`, `responses`, and `anthropic` (native Messages, which
authenticates with `x-api-key` and defaults to `https://api.anthropic.com/v1`):

```sh
export ANTHROPIC_API_KEY='...'
export ROAST_FINDER_MODEL='claude-model-name'
export ROAST_FINDER_PROTOCOL='anthropic'
```

For API-key protocols, any base URL works, so a role can point at a hosted provider, a gateway, or a
local server that implements the selected protocol.

### Config file

For named profiles, use `roast/config.yaml` in your user config directory (see
[Config file location](#config-file-location)) or pass `--config`:

```yaml
version: 1

inference_profiles:
  claude:
    endpoint: https://api.anthropic.com/v1
    model: claude-model-name
    protocol: anthropic
    api_key_env: ANTHROPIC_API_KEY
    max_output_tokens: 32768
  verifier:
    endpoint: https://verifier.example/v1
    model: verifier-model
    protocol: responses
    api_key_env: OPENAI_API_KEY
    max_output_tokens: 32768

roles:
  finder:
    profile: claude
  verifier:
    profile: verifier
```

A profile is a complete unit: model, `endpoint`, `protocol`, output-token limit,
and credential source. Roast never infers a protocol or credential from a model
name, and never reads configuration from the repository under review, so a
checkout cannot redirect inference or supply credentials.

`api_key_env` names an environment variable that is read only when the profile
is selected. A literal `api_key` also works, but environment references are
preferable. `version: 1` is required for new configs; Roast migrates its two
legacy unversioned formats in memory, and rejects unknown fields and
unsupported versions.

### Direct ChatGPT plan usage

This fork also has a separate `chatgpt` protocol implementing the official
[Sign in with ChatGPT plan-usage flow](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference).
Build this fork with Go 1.27.3 or newer (`go build -o roast ./cmd/roast`).
Direct authentication currently supports Linux and macOS. On Windows it fails
before reading credentials or starting OAuth, pending native secure-storage
support. The other Roast backends are unchanged.

```sh
./roast chatgpt signin
./roast chatgpt models
```

`signin` prints a **Continue with ChatGPT** authorization link. Open it in a
browser on the same computer. The browser asks you to name/authorize Roast and
approve plan access. Roast uses dynamic public-client registration, PKCE, a
random state and nonce, and a `127.0.0.1` callback. No API key, client secret,
partner registration, Codex installation, or Codex token extraction is needed.
Only run this command when you intend to grant Roast ongoing plan access.
Roast validates the signed ID token and granted scopes before using credentials.

Choose model slugs from your account's live `models` catalog, then configure:

```yaml
version: 1
inference_profiles:
  plan_finder:
    protocol: chatgpt
    model: gpt-6.1-sol  # replace with a slug available to your account
    max_output_tokens: 32768
  plan_verifier:
    protocol: chatgpt
    model: gpt-6-astra  # replace with a slug available to your account
    max_output_tokens: 32768
roles:
  finder:
    profile: plan_finder
  verifier:
    profile: plan_verifier
```

Run `roast doctor --config /path/to/config.yaml` to check the grant, current model
catalog, and an actual two-turn native function-tool loop for each role. A saved
login alone is not proof of eligibility. Account/workspace/region policy and
plan-usage limits still apply. There is no automatic API-key billing fallback.

The `chatgpt` protocol preserves Roast's native agent loop: its prompts become
Responses developer/user messages; its own scoped function tools are grouped
in the `roast` namespace; tool results and finalized raw assistant output,
including encrypted reasoning, are replayed in their original order. There is
no additional Codex CLI harness or Codex-native tool execution. Only a valid
`response.completed` permits dispatch; failed, incomplete, interrupted, or
wrong-namespace streams cannot execute buffered tools. Run capture retains the
original SSE bytes, and still must succeed before tool dispatch.

The endpoint is fixed to `POST https://api.openai.com/v1/responses`, with
`store: false` and `stream: true`. Profiles must omit `endpoint`, `api_key`,
`api_key_env`, and `session_header`; role endpoint/key overrides are rejected.
Ambient OpenAI keys, base URLs, organization/project IDs, and custom headers
are not used. Redirects are not followed. This backend never uses ChatGPT
`backend-api` routes. The documented preview's unsupported fields and hosted
tools are not sent; Roast currently sends text prompts and text tool results.

**Token budgets are soft limits on this route.** The preview rejects
`max_output_tokens`, so that profile setting estimates admission headroom only,
not a generation cap. An in-flight call can exceed Roast's token estimate.
HTTP request deadlines remain enforced. Usage-limit failures stop the request
and point to [ChatGPT usage settings](https://chatgpt.com/settings/usage), without
inventing a reset time. Temporary admission availability failures use bounded retries;
invalid capabilities and ineligible accounts are not retried unchanged.

#### Accounts and protected credentials

`roast chatgpt status` lists registrations without printing tokens.
`roast chatgpt select CLIENT_ID` selects an account for subsequent processes.
Each running model client pins its selected registration. Use
`roast chatgpt signin --account CLIENT_ID` to reauthorize that registration.
If identity sign-in succeeds without plan permission, Roast retains it but
blocks inference. Only an explicit
`roast chatgpt signin --account CLIENT_ID --enable-plan` asks for renewed consent.
`login` is an alias for `signin`.

Roast stores credentials separately under the operating-system user config
directory's `roast/chatgpt/` subdirectory (for example,
`~/.config/roast/chatgpt/` on Linux). `accounts.json` records the stable host ID,
selected client ID, and registration list. New hosts use a random UUIDv4 in
`urn:uuid:` format, reused across sign-ins. Legacy Roast-generated host IDs
are migrated only when no registration exists; registered host identities are
never silently replaced. Each credential filename is the
SHA-256 hex digest of its issued client ID, followed by `.json`. Directory and
file permissions are owner-only. Atomic writes and kernel-released locks
serialize rotating-token refreshes across processes. A pending rotated token
response is protected on disk before fallible validation, so a temporary JWKS
failure does not lose the replacement refresh token. Tokens are never placed
in Roast's YAML, run metadata, URLs printed for returning sign-in, or logs.

`roast chatgpt signout [CLIENT_ID]` clears local tokens and attempts server-side
revocation, retaining the registration identity for later sign-in. If revocation
cannot be confirmed, it reports that separately. Disconnecting Roast in ChatGPT
settings is not pushed to Roast; the next confirmed auth failure surfaces it.
Do not commit, paste, or upload the credential directory or copy Codex auth files.

For a self-hosted VM, follow the official
[VM procedure](https://developers.openai.com/siwc/token-sharing-open-source/self-hosted-vms):
run `roast chatgpt prepare-host` on the VM first, then complete OAuth locally
using Roast for the same user/workspace. The command prints the target store
path and its nonsecret host ID. Transfer only the selected Roast registration's
protected credential file over a secure channel such as SSH. Update the VM's
`accounts.json` registration list/active client while preserving its existing
`host_id`, and retain owner-only permissions. Do not replace the VM's host ID
with a laptop's ID. The VM must own subsequent refreshes; do not run concurrent
copies of the same transferred session on different hosts. This release does
not automate credential transfer. Host-specific usage attribution and revocation
for transferred sessions are not yet provided by the service.

See the official [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)
and [error recovery guide](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery).
The existing `codex` protocol below remains a separate fallback, not a substitute
for a live direct-route test.

### Codex with a ChatGPT login

This backend is specific to this fork; upstream Roast releases do not include
it. From this fork's checkout, build with Go 1.27.3 or newer using
`go build -o roast ./cmd/roast`, then use that binary (for example `./roast`).

Install the official [Codex CLI](https://developers.openai.com/codex/cli/),
then run `codex login` and choose ChatGPT sign-in. Roast uses the CLI's managed
login, not an OpenAI API key. Choose a model available to your ChatGPT account:

```yaml
version: 1
inference_profiles:
  chatgpt:
    protocol: codex
    model: gpt-model-name
    max_output_tokens: 32768
roles:
  finder:
    profile: chatgpt
  verifier:
    profile: chatgpt
```

Run `roast doctor --config /path/to/config.yaml` to check the CLI's ChatGPT
login and exercise the two-turn tool loop. The CLI must be on `PATH`.
Use a recent official CLI with `exec --ignore-user-config`, `--ephemeral`,
`--json`, and `--output-schema` support. Roast launches each call in a fresh
private temporary directory with the read-only sandbox. User configuration,
project instructions, native shell/browser/image tools, plugins, and hooks are
disabled. Roast runs its own scoped tools and rejects results containing
native-tool events. That check happens after CLI execution, so it is defense
in depth, not a guarantee that no native tool ran. Full conversation history is
replayed for each call, which can be slower and consume more of your subscription
allowance than a single interactive Codex session.

The CLI needs a writable runtime home even with ephemeral sessions. If your
host's existing Codex installation is read-only, use a separate writable
`CODEX_HOME` and complete the official `codex login --device-auth` flow there.
Do not copy authentication files or tokens from another installation. Roast
inherits `CODEX_HOME`, but never initiates login or creates credentials itself.
A Codex profile must omit `endpoint`, `api_key`, `api_key_env`, and
`session_header`. Role API-key and endpoint overrides are rejected too.
`OPENAI_API_KEY` and `OPENAI_BASE_URL` do not affect this protocol.
The provenance endpoint `codex://chatgpt` is a label, not an HTTP endpoint or
a configurable proxy. Credential kind `codex_login` means CLI-managed login;
configuration resolution alone does not verify login or account entitlement.
Roast never reads, extracts, or copies the CLI's login tokens.

For Codex, `max_output_tokens` is a conservative estimate for admitting a call
against Roast's token budget, not a hard generation ceiling. The CLI does not
expose that per-call cap. Roast's token budget is a soft limit and may be
exceeded by a call already in flight. Codex CLI/model limits and ChatGPT
account usage limits still apply. Doctor permits up to 120 seconds per call
and 250 seconds for each role's two-turn probe.

#### CLI harness limitations

This backend does **not** provide the same model interface or audit evidence as
Roast's direct API providers. Each Roast conversation, including its system-role
messages and tool schemas, is serialized inside a Codex **user prompt**. Those
quoted role labels do not retain native API instruction priority. Codex still
adds its own system/developer instructions, model-specific tool definitions,
and agent/policy behavior above that prompt. Ignoring user config does not
remove this built-in harness or centrally managed configuration.

Roast retains its outer finder/verifier workflow, scoped tool dispatch,
submission validation, deadlines, and local run records. However, Codex may
perform internal steps before returning the structured tool selection. Some
built-in tools can remain exposed, such as `apply_patch` (whose writes the
read-only sandbox denies) and model-specific utilities. The adapter's native
item rejection happens after execution, not before it.

Codex response capture stores the final tool envelope and reported usage, not
the original provider SSE, full harness prompts, or internal execution trace.
The recorded call count counts CLI invocations rather than underlying model
requests. Context overhead, latency, usage accounting, and output limits also
differ. A verified Roast report still means the outer verifier accepted the
finding, but this backend must not be presented as equivalent to upstream's
direct-model isolation, instruction hierarchy, or raw-response auditability.

### Credentials and precedence

Command-line role flags override `ROAST_FINDER_*` / `ROAST_VERIFIER_*`
variables, which override profiles. For API-key protocols, a role's credential is the first of:

1. `ROAST_FINDER_API_KEY` or `ROAST_VERIFIER_API_KEY`;
2. the profile's `api_key_env` or `api_key`;
3. the provider's `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`, but only when the
   role's endpoint is that provider's default or its `OPENAI_BASE_URL` /
   `ANTHROPIC_BASE_URL`.

Provider variables are fallbacks, never overrides: a provider `*_BASE_URL`
applies only to a profile with no `endpoint`, and the `OPENAI_*` variables
apply only to `chat` and `responses` while `ANTHROPIC_*` apply only to
`anthropic`. A profile with any other endpoint must name its credential, so a
key in your shell is never sent implicitly to a gateway or a configuration
template's endpoint. When a protocol override moves a role to another
provider's endpoint, the profile's credential does not follow it. Flags and
environment overrides follow the same rules, and `roast doctor` reports which
source each role used.

### Author routes

When the invoking agent passes `--author-model` (or `ROAST_AUTHOR_MODEL`), the
finder can switch to another profile by exact model name or model family, so a
model is not reviewing its own work. Here `primary` and `independent` are
profiles defined under `inference_profiles`:

```yaml
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
      exact-author-model: independent
```

`roles.finder.pin: true` keeps the configured finder anyway, with a warning.

### Other settings

| Key | Default | Effect |
| --- | --- | --- |
| `review.excludes` | none | Git pathspecs excluded from every review, in addition to `--exclude`. |
| `review.confidence_floor` | `0.6` | Findings below this finder confidence (0–1) are dropped before verification and listed under `dropped_findings`. `0` selects the default. |
| `review.quick_confidence_floor` | `0.8` | The same floor for `--quick` reviews, which skip verification. |
| `roles.finder.pin` | `false` | Ignore `author_routes` even when the author shares the finder's model family. |
| `inference_profiles.<name>.session_header` | none | Also send the per-conversation session ID under this header name, for gateways that key sessions on their own header (for example `x-opencode-session`). It must be a valid header name and cannot name a credential or framing header, or one Roast or the provider SDK sets. |
| `prompts.finder_code`, `prompts.finder_prose`, `prompts.verifier` | built in | Absolute paths to prompt overrides. |

Runs record the profile name and resolved non-secret settings, never API keys.
The invocation argv is recorded, so keep secrets out of command-line values.

### Config file location

Without `--config`, Roast reads `roast/config.yaml` from the user configuration
directory:

| Platform | Default path |
| --- | --- |
| macOS | `$XDG_CONFIG_HOME/roast/config.yaml` when set, otherwise `~/Library/Application Support/roast/config.yaml` |
| Linux and other Unix | `$XDG_CONFIG_HOME/roast/config.yaml`, or `~/.config/roast/config.yaml` when `XDG_CONFIG_HOME` is unset |
| Windows | `%AppData%\roast\config.yaml` |

Windows ignores `XDG_CONFIG_HOME`. On macOS and other Unix platforms, a non-empty
`XDG_CONFIG_HOME` must be an absolute path; unset or empty keeps the platform's
default. An explicit `--config` takes precedence. A missing default file is not
an error. `roast doctor` reports the path it used.

On macOS, this also changes the selected path if you already export
`XDG_CONFIG_HOME`. See the [upgrade guide](docs/upgrading.md#2-initialize-configuration)
before upgrading an existing installation.

To keep your configuration under `~/.config` on macOS, add this to your shell's
startup file:

```sh
export XDG_CONFIG_HOME="$HOME/.config"
```

### Configuration URLs

An organization can publish a secret-free configuration document, installed
with `roast config init --from <https-url>`. The document must declare
`version: 1`, may name credential environment variables, and must not contain
credentials or control characters. A template can still name a variable such
as `OPENAI_API_KEY` for its own endpoint, which sends that key there, so Roast
prints a summary of each selected profile's endpoint and credential variable,
and of any upload destination, when it installs a template.

When stdin and stderr are terminals and no `--config` is given, `roast review`
and `roast runs sync` also offer this prompt if the default config is missing
and the roles cannot be resolved from the environment or flags, or if the
config is unversioned or has Claude profiles using Chat Completions or
Responses. A [presigned upload broker](#presigned-upload-broker) can serve the
document.

## Use

Run Roast inside a Git repository:

```sh
roast review --task 'add retry handling'   # review the current changes
roast review --staged --max-severity HIGH  # staged changes; exit 1 on HIGH+
roast review --interactive                 # let the finder navigate the repo
roast review --quick                       # finder only, no verification
roast review --scope repo                  # survey the whole repository
roast runs                                 # list local runs
roast runs show <run-id>
roast runs checkout <run-id>
roast doctor
```

The default scope is committed changes since the best available main branch,
plus uncommitted and untracked changes. Untracked files that look like secrets
(`.env*`, `*.pem`, `*.key`, SSH keys such as `id_ed25519*`, and similar) are
skipped with a note on stderr. JSON goes to stdout and Markdown to stderr;
`--format` selects one.

`--interactive` gives the finder several turns with the navigation tools. Small
diffs (under 80 lines and 4 files) get a single structured call instead, and
Roast says so; `ROAST_INTERACTIVE_MIN_DIFF_LINES` and
`ROAST_INTERACTIVE_MIN_FILES` change those thresholds, and `0` disables them.
`--quick` turns navigation off. `--scope repo` always navigates and rejects
`--quick`.

The finder and verifier must submit schema-validated results through a tool
call. A role that never does gets bounded reminders within its limits; if it
still fails, the run is reported incomplete, never clean.

`roast doctor` validates profiles and credentials, lists each provider's
models, runs a small two-turn tool loop per configured model (a tool call, its
result, and the final submission, as a review does), and reports
Git, gate, Agent Skill, upload, and release status. `--json` gives
machine-readable output.

### What is sent where

Source code, diffs, prompts, tool schemas, and tool results go to the inference
endpoints you configure, and run bundles go to your upload destination if you
enable uploads. Review data goes nowhere else; Roast's only other network calls
are the release check and configuration URLs you ask it to fetch.
Review your providers' data-handling terms before using Roast on sensitive
code. The [security model](docs/security-model.md) covers trust boundaries and
artifacts in full.

Each request carries an `X-Agent-Session-Id` that is stable across one model
conversation's tool turns and retries and differs between conversations. It
identifies a conversation, not a person or a whole run. A profile's
`session_header` sends the same value under one more header name as well.

## Resource limits

Defaults are generous enough for slow local models, and every tunable limit is
yours to change:

| Control | Default | Override |
| --- | ---: | --- |
| Output tokens per model call | 32,768 | Profile `max_output_tokens`, `ROAST_{FINDER,VERIFIER}_MAX_OUTPUT_TOKENS`, or `--{finder,verifier}-max-output-tokens` |
| Timeout per inference request | 10 minutes | `--request-timeout` / `ROAST_REQUEST_TIMEOUT`; `0` disables |
| Interactive finder work deadline | 10 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Repository-survey work deadline | 25 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Provider/structured-output attempts | 4 | `--max-attempts` / `ROAST_MAX_ATTEMPTS` |
| Shared finder/verifier token budget | 3,000,000 | `--max-tokens-total` / `ROAST_MAX_TOTAL_TOKENS`; `0` disables |
| Findings sent to the verifier | 25 | `--max-verified` / `ROAST_MAX_VERIFIED`; `0` disables |
| Diff bytes, normal / quick (also caps each file's section) | 1,000,000 / 200,000 | `ROAST_MAX_DIFF_BYTES` |
| File-context bytes, normal / quick | 400,000 / 100,000 | `ROAST_MAX_FILE_CTX_BYTES` |
| Initial finder prompt bytes | 8,000,000 | `ROAST_MAX_PROMPT_BYTES` |

Interactive runs default to 16 finder turns and surveys to 40 (`--max-turns`).
Past roughly 3 MB of conversation, Roast tells the finder to stop exploring and
submit; a work deadline leaves a separate 90-second window for that final
submission.

Fixed caps protect memory and artifacts: 16 MiB per captured provider response;
64 KiB per `git_diff` window and per `read_file`, `grep_repo`, or `list_files`
result; `read_file` lines truncated at 32 KiB, 2,000 lines per window;
`grep_repo` lines truncated at 2 KiB; at most 500 listed paths and 200 matching
lines.

Invalid or negative overrides are usage errors, not silent fallbacks. Resolved
limits are recorded with each run.

## Exit codes

| Code | Meaning | `error_category` |
| ---: | --- | --- |
| 0 | Clean completion | `none` |
| 1 | A finding met the `--max-severity` threshold | `findings` |
| 2 | Invalid invocation or configuration | `usage` |
| 3 | Inference authentication failed | `auth` |
| 4 | Operational failure or incomplete result | `endpoint`, `resource_limit`, or `partial` |
| 5 | Cancelled or terminated | `cancelled` |

`--error-format json` writes diagnostics to stderr as JSON without changing
review output:

```json
{"error_category":"endpoint","message":"...","exit_code":4}
```

## Run records

Every non-empty review is recorded under `$XDG_DATA_HOME/roast` (default
`~/.local/share/roast`; override with `ROAST_RECORD_DIR`). A run holds the
secret-free role configuration, argv, exact prompts and reviewed input, raw
provider response bodies (streamed SSE kept byte-for-byte, including partial
streams from failed attempts), usage, results, a Git snapshot, and a
deterministic `bundle.tar.zst` whose manifest lists every artifact's size and
SHA-256. Requests, HTTP headers, and credentials are never recorded. Run
directories are kept until you delete them.

The navigation tools read the snapshot, not the live checkout: the captured
index for `--staged`, otherwise the captured worktree including non-ignored
untracked files. Editing files mid-review changes nothing the models see.

Snapshots are Git refs under `refs/roast/snapshots/`. `roast runs checkout
<run-id> [dir]` restores one as a detached linked worktree, with the reviewed
contents on disk and the reviewed index staged, so `git status` and `git diff`
show exactly what was reviewed. `roast runs prune` removes snapshot refs older
than 30 days (`--dry-run` to preview) but never run directories.
`ROAST_SNAPSHOT=0` skips snapshot capture, so such a run cannot be checked out.

## Run uploads

Uploads are off by default. When enabled, Roast uploads each completed review's
bundle on a best-effort basis; failures never change the review result and stay
pending for `roast runs sync`. `roast runs status` shows upload state. Runs
recorded before you opted in are uploaded only with
`roast runs sync --include-existing`. Running `roast runs sync` is itself an
explicit upload request, so it uploads even while automatic upload is disabled.

To upload directly to an S3-compatible store (AWS S3, MinIO, Cloudflare R2, and
others):

```yaml
upload:
  enabled: true
  backend: s3
  endpoint: https://objects.example.com
  bucket: roast-runs
  prefix: team-name
  region: us-east-1
  access_key_env: ROAST_UPLOAD_ACCESS_KEY
  secret_key_env: ROAST_UPLOAD_SECRET_KEY
  # session_token_env: ROAST_UPLOAD_SESSION_TOKEN
  path_style: false
```

Credentials come only from the named environment variables, never from AWS
profiles, SSO, or instance metadata. Objects are content-addressed at
`<prefix>/sha256/<bundle-digest>.tar.zst`. Endpoints must use HTTPS unless on
loopback; `allow_insecure_http: true` permits plaintext on a trusted local
network and must never be used across an untrusted one.

Organizations that don't hand out storage credentials can run a
[presigned upload broker](#presigned-upload-broker) instead:

```yaml
upload:
  enabled: true
  backend: presigned
  endpoint: https://roast-uploads.example.com
  token_env: ROAST_UPLOAD_TOKEN   # or a literal upload.token in your own config
```

The bearer token goes only to the broker. The broker returns a short-lived,
content-bound PUT URL; Roast uploads the bundle straight to storage without the
token, then asks the broker to verify it.

## Presigned upload broker

`roast-broker` is a small stateless service, built by the root `Dockerfile`. It
relies on a trusted reverse proxy for authentication: on `/api/v1/roast/runs`
and its completion endpoint, the proxy must strip any incoming principal
header, validate `Authorization: Bearer`, and inject the verified principal.
`/healthz` and the optional `/config/v1` are unauthenticated so new clients can
fetch configuration. Never expose the container directly; every route shares
one listener, and the API trusts the principal header.

Required environment:

```text
ROAST_BROKER_PRINCIPAL_HEADER=X-Forwarded-Email
ROAST_BROKER_SIGNING_KEY=<at least 32 random bytes>
ROAST_BROKER_S3_ENDPOINT=https://objects.example.com
ROAST_BROKER_S3_BUCKET=roast-runs
```

Storage credentials come from `ROAST_BROKER_S3_ACCESS_KEY`,
`ROAST_BROKER_S3_SECRET_KEY`, and optional `ROAST_BROKER_S3_SESSION_TOKEN`, or
from the MinIO client's IAM/workload-identity chain when those are unset. Also
available: `ROAST_BROKER_S3_REGION`, `ROAST_BROKER_S3_PREFIX`,
`ROAST_BROKER_S3_PATH_STYLE`, `ROAST_BROKER_MAX_BUNDLE_BYTES`, and
`ROAST_BROKER_LISTEN_ADDRESS` (default `:8080`). Set
`ROAST_BROKER_CLIENT_CONFIG_FILE` to a validated, secret-free client config to
serve it at `/config/v1` as a configuration URL; startup fails if it is
unversioned, invalid, or contains credentials.

The protocol has three steps:

1. `POST /api/v1/roast/runs` declares the run ID, bundle size, and SHA-256, and
   returns a presigned PUT and an opaque completion token.
2. The client PUTs the bundle with exactly the returned headers and no broker
   credential.
3. `POST /api/v1/roast/runs/{run_id}/complete` presents the token; the broker
   checks ownership and the stored object's size and checksum.

Each grant gets a unique object key, so the broker needs no database. Configure
bucket lifecycle expiration to clean up abandoned uploads.

## Pre-commit gate

```sh
roast gate enable
roast gate status
roast gate disable
```

The gate runs a `--quick` review of the staged content on each commit and
blocks only when a finding meets the severity threshold. It fails open: a
missing binary, failed inference, empty output, or timeout prints a warning and
allows the commit.

The first interactive review in a repository asks once whether to enable it and
records the answer in local Git config; non-interactive runs never install a
hook without that consent. `ROAST_NO_HOOK=1` skips the question. With a shared
`core.hooksPath`, each repository still needs its own opt-in, and disabling one
leaves the dispatcher in place for the others.

| Variable | Effect |
| --- | --- |
| `ROAST_SKIP` | Any non-empty value (conventionally `1`) skips one commit. |
| `ROAST_BIN` | The `roast` binary to run instead of the one on `PATH`. |
| `ROAST_GATE_TIMEOUT` | Seconds before an unfinished review is stopped and the commit allowed. Default `180`; `0` disables; a non-numeric value uses the default. |
| `ROAST_GATE_SEVERITY` | Lowest blocking severity: `INFO`, `LOW`, `MEDIUM`, `HIGH` (default), or `CRITICAL`. An invalid value fails the review, so the commit is allowed. |

## Contributing

Pull requests are squash-merged, so each title becomes a commit on `main` and a
release-notes entry. Titles need a conventional `type:` prefix (checked in CI):
`feat:` releases a minor version, `feat!:` or a `BREAKING CHANGE:` footer a
major version, and any other type, such as `fix:` or `docs:`, a patch. See
[CONTRIBUTING.md](CONTRIBUTING.md) for development checks and release steps,
and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

Dual-licensed under [MIT](LICENSE-MIT.txt) or
[Apache 2.0](LICENSE-APACHE.txt), at your option. Dependencies keep their own
licenses; CI checks the linked dependency graph against the allowed set.
