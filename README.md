# Roast + ChatGPT

This fork of [Roast](https://github.com/bluesky-social/roast) adds code reviews
using your ChatGPT plan through OpenAI's public API, with its own OAuth sign-in.
It does not use the Codex CLI or Codex credentials.

**Experimental:** live end-to-end verification is incomplete. Not production-ready.
Requires Linux or macOS and Go 1.27.3+.

## Install

```sh
git clone https://github.com/solimaniac/roast.git
cd roast
go build -o roast ./cmd/roast
export PATH="$PWD:$PATH"
roast chatgpt signin
roast chatgpt models
```

Open the sign-in link in a browser on the same computer. Approval grants ongoing
plan access. Use model slugs from that catalog.

## Use

Replace the placeholders, then run from the repository you want reviewed:

```sh
export ROAST_FINDER_PROTOCOL=chatgpt ROAST_VERIFIER_PROTOCOL=chatgpt
export ROAST_FINDER_MODEL='<finder-model-slug>'
export ROAST_VERIFIER_MODEL='<verifier-model-slug>'
cd /path/to/your/repository
roast doctor
roast review --task 'Describe the change'
```

For this backend, remove endpoint, API-key, and session-header settings from
existing profiles, plus role endpoint/key overrides.

`doctor` and reviews consume plan usage; reviews send code to OpenAI. Eligibility
and plan limits apply, with no API-key billing fallback. `max_output_tokens` is
not a hard cap. Keep credentials and authorization URLs private.

Use `roast chatgpt signout` to clear local tokens and attempt remote revocation.

For general usage, configuration, and workflows, see the
[upstream documentation](https://github.com/bluesky-social/roast#readme).
