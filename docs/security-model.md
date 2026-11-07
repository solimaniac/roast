# Security model

Roast reviews source code by sending selected repository material to inference
services. This document describes the boundaries operators should understand;
it is not a promise that a model or provider will handle untrusted input safely.

## Trusted configuration

Inference profiles, credentials, upload destinations, and prompt overrides come
from the user's Roast configuration, environment, or command-line flags. Roast
does not load repository-local configuration. A checked-out repository therefore
cannot select a network destination or credential merely by adding a config
file.

## Direct ChatGPT plan usage

The opt-in `chatgpt` protocol is a direct Responses provider, separate from the
`codex` CLI adapter. It addresses the architectural concerns described in
[upstream issue 127](https://github.com/bluesky-social/roast/issues/127#issuecomment-6693962921):
Roast retains its own agent loop, scoped read-only exploration tools, final
submission schema validation, and raw provider-response records. It does not
launch Codex or delegate the review to a general-purpose CLI harness.

Roast sends its prompts as native developer/user messages and exposes only its
local function tools in the `roast` namespace. It preserves finalized raw
assistant output, including encrypted reasoning, in conversation replay. Tools
remain buffered until a valid `response.completed` and successful raw SSE
capture. Failed/incomplete/interrupted streams, other tool namespaces, and
unsupported output types stop the turn without dispatching buffered tools.
The existing snapshot/path boundary is unchanged: when Git tree pinning fails,
Roast's existing warned fallback reads the live tree under path guards. This
backend does not promise a captured snapshot in that fallback case.

The OAuth access token is sent only to the fixed public OpenAI model-list and
Responses endpoints. Requests use `store: false`, `stream: true`, and full input
history. Endpoint overrides, API-key overrides, ambient credential headers,
redirects, and automatic alternative billing are not allowed. Ordinary OpenAI
service policies and account/workspace restrictions still apply.

Explicit `roast chatgpt signin` creates a separate public-client OAuth grant.
PKCE, state, nonce, ID-token signature/identity checks, and granted scopes protect
registration. Roast owns these credentials under its private config directory;
it never reads Codex credentials. Atomic owner-only files, OS-released locks,
and protected pending refresh responses handle rotation and interruption.
Returning sign-in does not force consent unless the user requests `--enable-plan`.
Direct auth currently rejects Windows before credential access because native
ACL protection is not implemented. VM credential transfer is manual and must
preserve the target host identity; see the README and official VM guide.

The preview rejects `max_output_tokens`: output estimates and shared token
budgets are soft, while request deadlines still apply. A saved grant and mocked
tests do not prove a particular account/model is eligible. Run the separately
authorized two-turn `roast doctor` or opt-in live test to verify it. The retained
`codex` fallback below does not gain these direct-provider properties.

## Codex CLI and ChatGPT login

The opt-in `codex` protocol sends review prompts and selected repository
material through the official Codex CLI using its ChatGPT login. The CLI and
its installation are trusted dependencies. Roast does not extract or copy
OAuth/session tokens, and does not use API credentials for this route.
`codex://chatgpt` records this CLI-managed destination in provenance; it is not
an HTTP service or a user-configurable endpoint. `codex_login` describes the
credential mechanism without claiming that a login has been verified.
`roast doctor` asks the CLI to check login and runs a synthetic two-turn probe.

Each inference subprocess receives an allowlisted operating-system environment,
a fresh private temporary working directory, no user config or project docs,
read-only sandboxing, and disabled shell, browser, image, plugin, and hook
features. Roast sends the prompt on stdin instead of command-line arguments.
Subprocess output is bounded, raw diagnostics are never surfaced, and native
tool events or incomplete responses are rejected before Roast dispatches a tool.
Run capture stores only the final tool envelope and usage, not CLI session IDs
or raw diagnostic events. Temporary prompt/schema/runtime artifacts are removed
at the end of the call. The CLI remains responsible for its login storage and
may need to update its own runtime home; use a writable installation.

Codex profiles reject explicit endpoints, API keys, key environment variables,
and session headers. Endpoint and role API-key overrides are rejected;
ambient OpenAI API credentials and base URLs are ignored. Set up login with
`codex login`, outside Roast. ChatGPT plan/model access and usage limits apply.
For this protocol `max_output_tokens` is only an admission estimate: the CLI
cannot enforce Roast's output-token ceiling, and token budgets are soft limits.

### The CLI remains a separate agent harness

The adapter does not call the raw model directly. It serializes Roast's original
message roles, history, and tool schemas into one CLI user prompt. A serialized
`system` label is data, not a native API system instruction. Codex's own
system/developer instructions, policy machinery, model-specific tools, and
centrally managed settings remain in effect and may change with CLI versions.
This changes the instruction hierarchy and adds a trust boundary compared with
Roast's direct HTTP providers.

Disabled feature flags and read-only sandboxing reduce capability but do not
remove all native tool definitions. For example, Codex can expose `apply_patch`
while the sandbox denies writes, and model-specific utility tools can remain.
Rejecting an unexpected/native item prevents subsequent Roast tool dispatch;
it cannot undo or prove the absence of an action the CLI already attempted.
Do not treat the adapter's post-execution event checks as a pre-execution tool
allowlist or a replacement for the CLI's sandbox.

For `protocol: codex`, durable response capture is synthesized from the final
structured envelope and CLI-reported usage. It omits original provider SSE,
internal prompts, and internal agent steps. A usage `calls` count is a count of
CLI invocations, not a count of underlying model requests. Consequently this
backend preserves Roast's outer review/verification workflow but does not claim
identical adversarial isolation, instruction priority, accounting, or audit
replay guarantees to a direct-model provider.

## Configuration templates

An organization can publish a configuration template at a URL. A template is
installed by `roast config init --from <url>`, by `roast config init` with the
URL typed at its prompt, or by entering the URL at the interactive
configuration-URL prompt that `roast review` and `roast runs sync` can show
(described below). Templates are trusted input: review and host them with the
same care as other organization configuration.

The configuration URL must use HTTPS, or plain HTTP to a loopback address, and
must not embed credentials or a fragment. Redirects must stay on the same
origin, and the document is limited to 1 MiB. Roast validates the complete
template before writing anything, installs it atomically with owner-only
(`0600`) permissions, and never sends a local credential to the configuration
URL. A template that only switches existing Claude profiles to the native
`anthropic` protocol changes nothing else in an existing configuration, and the
previous file is kept as a backup.

A template may set:

- inference profiles: model, endpoint, protocol, output-token limit, and the
  name of the environment variable that holds each profile's credential
  (`api_key_env`);
- role selection and author routes;
- review defaults;
- upload settings, including enabling automatic uploads, the destination, and
  the names of the environment variables that hold upload credentials
  (`access_key_env`, `secret_key_env`, `session_token_env`, `token_env`).

A template may not contain a literal `api_key` or upload `token`, and may not
set `prompts.*`: prompt overrides name local files whose contents would be sent
to the template's endpoints, so they belong only in the user's own config.

Because a template can name any environment variable as a credential source and
can direct reviewed code and uploads to any endpoint, Roast prints a summary to
stderr whenever it installs one: for each role (and author route), the profile,
endpoint, protocol, and credential variable, plus whether uploads are on and
where they go. When a template enables uploads during an interactive install,
Roast installs it only after an explicit `y` answer to a `[y/N]` question.
Non-interactive installs print the summary and proceed, since the command
itself names the URL.

A template's text fields, such as profile names, author-route keys, endpoints,
and the upload bucket and prefix, must not contain control characters; Roast
rejects such a template before writing anything. The summary and the upload
question also escape control characters in every configuration value they
print, so configuration text cannot rewrite what the user is asked to approve.

The configuration-URL prompt appears only when both stdin and stderr are
terminals and no `--config` path is given. It then appears in three cases: the
default config file is missing and the finder and verifier cannot be resolved
from the environment and command-line flags; the file is unversioned; or the
file has Claude profiles using Chat Completions or Responses. The last two cases
prompt even when the roles already resolve. Leaving the URL blank leaves the
configuration as it is; for a versioned file, that choice is recorded in a
private sibling `.protocol-update-ack` file containing only the config's hash,
so the prompt does not repeat until the config changes. Its prompts go to
stderr, so redirected review output is never mixed with them.

## Credentials and endpoints

Each inference profile binds a model, endpoint, protocol, and credential
source as one unit. Provider environment variables are a fallback, never an
override:

- `ROAST_FINDER_*` and `ROAST_VERIFIER_*` variables override the selected
  profile's fields, and command-line role flags override both.
- `OPENAI_BASE_URL` (for `chat` and `responses`) or `ANTHROPIC_BASE_URL` (for
  `anthropic`) applies only to a profile that sets no `endpoint`, including
  after a protocol override moves that profile to another provider.
- A role's credential is the first of: `ROAST_FINDER_API_KEY` or
  `ROAST_VERIFIER_API_KEY`; the profile's `api_key_env` or `api_key`; or the
  provider's `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`, but only when the role's
  endpoint is that provider's default endpoint or the same provider's
  `*_BASE_URL`.
- When a protocol override moves a role from one provider's endpoint to
  another's, the profile's credential does not follow it.
- An endpoint override (`ROAST_<ROLE>_BASE_URL` or `--<role>-base-url`)
  carries the role key or the profile's own credential to the endpoint it
  names. It carries a provider key from the shell only under the rule above:
  when the endpoint it names is that provider's default endpoint or the same
  provider's `*_BASE_URL`.
- `ROAST_<ROLE>_*` variables and the matching command-line flags apply these
  rules identically.

A provider key in the user's shell is therefore never sent implicitly to a
profile's custom endpoint, such as a gateway or a template's endpoint. A
profile with such an endpoint must name its credential, and the name is not
restricted: a profile, including one installed from a template, may set
`endpoint: https://models.example.org/v1` with `api_key_env: OPENAI_API_KEY`,
and that key is then sent to that endpoint. The install summary lists the
credential variable of each profile that a role or author route selects, and
the upload credential variables, so review it before relying on a template.
`roast doctor` reports which source each role used, and names a provider key it
withheld.

## Network disclosure

A review sends its prompt, selected diff or repository manifest, file content,
tool schemas, and local tool results to the configured finder endpoint. Candidate
findings and the code needed to adjudicate them are sent to the configured
verifier endpoint. `roast doctor` makes two small inference requests to each
configured role (a fixed probe prompt, one tool call and its result, then a
submission) in addition to querying its model-listing endpoint.

Each inference request carries only the credential, base URL, and headers from
Roast's resolved configuration. Roast ignores the provider SDKs' ambient
defaults, such as `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, Anthropic
profile files, `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`, and
`OPENAI_CUSTOM_HEADERS`; `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and the
`*_BASE_URL` variables apply only as inputs to Roast's own configuration.
A profile's `session_header`, which a template may also set, only adds one more
header carrying the per-conversation session ID; it cannot name a credential or
framing header, or one Roast or the provider SDK sets.

Roast does not infer trust from a model name. An OpenAI-compatible gateway,
hosted provider, and local server are all treated as the operator-configured
destination. Operators are responsible for checking those services' retention,
training, and access-control policies before reviewing sensitive repositories.

Run uploads are disabled by default. When enabled, Roast sends a deterministic
bundle containing the complete local run record to the configured S3-compatible
endpoint or presigned-upload broker. Upload endpoints, and the presigned URLs a
broker returns, must use HTTPS unless they are on loopback or
`upload.allow_insecure_http` is set. Failed uploads record the destination
without a presigned URL's signed query string. The bundled broker expects
authentication to be enforced by a trusted reverse proxy; it must not be
exposed directly to untrusted clients.

Builds that report a version other than `dev` check GitHub for a newer stable
release by sending an unauthenticated `GET` to the repository's latest-release
API endpoint, with Roast's version in the `User-Agent` header. These include
release binaries, `go install` builds, and source builds from a Git checkout,
which the Go toolchain stamps with a version. The check sends no review data,
repository content, or credentials. Commands reuse a cached result for up to a
day, `roast doctor` checks on each run, builds that report `dev` skip the check,
and `ROAST_NO_UPDATE_CHECK=1` disables it.

## Local sensitive data

Every non-empty review records the reviewed input, prompts, raw successful
provider responses, findings, report, metadata, and a Git snapshot used for
reproduction. These artifacts may contain proprietary source code, secrets
already present in reviewed content, or sensitive model output. Protect the
Roast data directory and repository's `refs/roast/snapshots/*` with the same
controls as the repository itself, and prune them according to local policy.

On Unix, Roast creates managed run directories with owner-only (`0700`)
permissions and artifacts, monthly records, and bundles with `0600` permissions.
Existing managed directories and files are tightened when written or reused.
New bundle entries also use `0600`; cached bundles retain their original bytes
and digest, so older archive entries may still carry their original permissions.
Roast does not change permissions on an existing configured data root or its
ancestors, and Unix permission bits do not establish Windows ACL isolation.

Known credential environment values are excluded from run metadata. Roast
cannot guarantee that a repository, model, provider, or tool result will not
echo a secret into an artifact or response.

## Untrusted repository and model input

Repository paths supplied by a model are confined beneath the reviewed tree;
absolute paths, traversal, unsafe Git revision spellings, escaping symlinks,
and paths inside `.git` are rejected. Every review reads a pinned Git tree, even
when no replayable snapshot can be recorded, so concurrent checkout changes
cannot alter the evidence seen during a run. If Git cannot write that tree,
Roast reads the live working tree instead but refuses ignored files and
untracked files matching its secret denylist.

Model output is schema-validated. Findings without a source quote grounded in
the reviewed corpus are dropped, and a separately configured verifier checks
surviving claims. These checks reduce accidental fabrication; they do not make
model output trusted executable input.

## Git hooks

On the first interactive review in a repository, Roast asks whether to install
its pre-commit gate. The answer is stored in that repository's local Git config,
and a decline is not prompted again. Non-interactive runs never install a hook
without consent. Roast does not overwrite foreign hooks: compatible shell hooks
receive a delimited invocation block, while incompatible hooks require a manual
step.

Gate dispatch requires this repository's local `roast.gate` opt-in, even when
`core.hooksPath` points to a shared directory. An inherited dispatcher is not
consent. Disabling a repository with a custom hooks path records a local decline
and preserves the dispatcher and foreign hooks for other repositories. Existing
Roast-owned dispatchers are refreshed to enforce this check when encountered;
foreign scripts are not rewritten.

The gate reviews staged content and fails open on operational errors. Use
`roast gate status`, `roast gate enable`, and `roast gate disable` to inspect or
change enrollment. `ROAST_SKIP=1` bypasses one commit.

## Vulnerability reports

See [the security policy](../SECURITY.md) for supported versions and private
reporting instructions.
