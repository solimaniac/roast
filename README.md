# Roast with ChatGPT

Upstream Roast has two review roles. The finder looks for bugs in your Git changes and suggests fixes. In a normal review, the verifier checks proposed findings against the code. Each role can use its own model. [Upstream Roast guide](https://github.com/bluesky-social/roast#readme)

**This fork adds experimental ChatGPT sign-in.** One or both roles can use your eligible ChatGPT subscription without an OpenAI API key. Roast saves its own login on your computer. [Compare this fork with upstream](https://github.com/bluesky-social/roast/compare/main...solimaniac:main)

## Install

Install with Git and Go 1.27.4 or newer. This replaces any `roast` in Go’s install directory. To keep that binary, set `GOBIN` to a separate absolute directory first.

```sh
git clone https://github.com/solimaniac/roast.git
cd roast
go install ./cmd/roast
```

Put Go’s install directory first on your `PATH`. Find it with `go env GOBIN`. If that’s empty, use the `bin` directory under the first path from `go env GOPATH`.

## ChatGPT plan

Run `roast chatgpt signin` and open the private link on the same computer to grant plan access. Use `roast chatgpt models` to list your model IDs.

<details>
<summary>Manage a saved account</summary>

- Use the client ID from `roast chatgpt status` for `CLIENT_ID` below.
- Switch accounts with `roast chatgpt select CLIENT_ID`.
- Grant missing plan access with `roast chatgpt signin --account CLIENT_ID --enable-plan`.
- Sign out with `roast chatgpt signout`. It clears the active account’s local tokens and tries to revoke them with OpenAI. [OpenAI account and session guide](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions)

</details>

### Config file location

By default, Roast reads `roast/config.yaml` under `~/Library/Application Support` on macOS, `~/.config` on Linux, or `%AppData%` on Windows. On macOS and Linux, a nonempty `XDG_CONFIG_HOME` overrides this location and must be absolute. To use another file, pass its absolute path with `--config` to both `roast doctor` and `roast review`.

For a new config, create its parent directory if needed and save this example at the chosen path. Leave `endpoint`, `api_key`, `api_key_env`, and `session_header` unset in ChatGPT profiles.

```yaml
version: 1
inference_profiles:
  # Replace these example model IDs with IDs from `roast chatgpt models`.
  sol: {protocol: chatgpt, model: gpt-6.2-sol}
  astra: {protocol: chatgpt, model: gpt-6.2-astra}
roles:
  finder: {profile: sol}
  verifier: {profile: astra}
```

<details>
<summary>Use ChatGPT with an existing config</summary>

In a version 1 config, add profiles with unused names under `inference_profiles`. Update the `profile` of each role you want to switch. If you set `--author-model` or `ROAST_AUTHOR_MODEL`, check which finder profile its route selects. [Upstream configuration guide](https://github.com/bluesky-social/roast#configure) · [Upgrade guide for older configs](docs/upgrading.md)

</details>

## Run a review

Clear conflicting role flags and `ROAST_FINDER_*` or `ROAST_VERIFIER_*` environment settings, including old API keys. For this example, leave `--author-model` and `ROAST_AUTHOR_MODEL` unset. Run `roast doctor`. Check that both roles pass and each ChatGPT role shows `chatgpt` and your chosen model. ChatGPT checks and reviews use your plan.

Change to the Git repository you want reviewed. Stage your changes and preview everything staged with `git diff --cached`. Use a repository you can share with both providers. The finder’s provider receives the patch, and the verifier’s receives findings. Both can receive supporting code outside the patch and tool results. ChatGPT roles use OpenAI.

Roast saves non-empty reviews locally. Automatic uploads are off by default. If `upload.enabled` is on, Roast tries to send each completed run’s bundle, including reviewed code, prompts, and responses, to your configured destination. Check delivery with `roast runs status`.

<details>
<summary>What Roast records and how plan limits work</summary>

By default, `--staged` keeps a local Git snapshot of the full index, including files outside the patch. Run records include the reviewed input, prompts, responses, snapshot references, and command-line arguments. Keep secrets out of arguments such as `--task`. Running `roast runs sync` explicitly requests uploads to your configured destination. For ChatGPT, `max_output_tokens` is a local budget estimate. Responses can be longer. [Run records](https://github.com/bluesky-social/roast#run-records) · [Security model](docs/security-model.md)

</details>

Describe the purpose of the change in `--task`.

```sh
roast review --staged --format markdown --task "Stop retrying after three failed attempts"
```

Find past reviews with `roast runs` and read a report with `roast runs show RUN_ID`. Check that the review completed. If it finished with no findings, the finder may have had no candidates to verify or the verifier may have rejected them all.

Without `--staged`, Roast includes uncommitted edits, eligible untracked files, and committed changes since the first base it finds (`origin/main`, `origin/master`, `main`, `master`, then `HEAD~1`). Use `--base REF` to choose the base.

[Review modes](https://github.com/bluesky-social/roast#use) · [Limits](https://github.com/bluesky-social/roast#resource-limits) · [Exit codes](https://github.com/bluesky-social/roast#exit-codes) · [Uploads](https://github.com/bluesky-social/roast#run-uploads) · [Pre-commit gate](https://github.com/bluesky-social/roast#pre-commit-gate) · [Agent skill](https://github.com/bluesky-social/roast#agent-skill)

## Contributing

Use a Conventional Commit title for pull requests. [Contributor guide](CONTRIBUTING.md) · [License (MIT or Apache 2.0)](LICENSE.txt)
