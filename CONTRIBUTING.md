# Contributing

Thanks for helping improve Roast. Bug reports and focused pull requests are
welcome. For large features, new dependencies, inference protocols, or changes
to artifact formats, open an issue before writing the implementation so the
contract can be agreed on first.

## Development

Roast requires Go 1.27.3 or newer. Clone the repository, then run:

```sh
go test -race ./...
go vet ./...
go tool -modfile=.github/tools/staticcheck/go.mod staticcheck ./...
go build ./cmd/roast
```

CI-only Go tools have separate modules under `.github/tools/`, maintained by
Renovate without adding them to Roast's runtime dependencies. Use those modules
locally too; a separately installed version can have a different Go importer.
Run the workflow and shell checks with:

```sh
go tool -modfile=.github/tools/actionlint/go.mod actionlint
shellcheck scripts/*.sh
goreleaser check
```

Format Go changes with `gofmt`. Tests use local HTTP servers and temporary Git
repositories; they must not require real model credentials or send repository
content to an inference provider.

Opt-in live tests in `internal/llm` run a minimal two-turn tool loop (one
exploration call, its result, then the submission on the wrap-up turn) against
a real endpoint with a small output ceiling. They send a fixed probe prompt,
never repository content, and skip unless their API key variable is set:

| Variable | Default |
| --- | --- |
| `ROAST_LIVE_ANTHROPIC_API_KEY` | unset (skip) |
| `ROAST_LIVE_ANTHROPIC_BASE_URL` | `https://api.anthropic.com/v1` |
| `ROAST_LIVE_ANTHROPIC_MODELS` | `claude-sonnet-5-5` (comma-separated) |
| `ROAST_LIVE_DEEPSEEK_API_KEY` | unset (skip) |
| `ROAST_LIVE_DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` |
| `ROAST_LIVE_DEEPSEEK_MODEL` | `deepseek-v4-pro` |
| `ROAST_LIVE_DEEPSEEK_PROTOCOL` | `chat` (or `anthropic` with an `/anthropic/v1` base URL) |

```sh
ROAST_LIVE_ANTHROPIC_API_KEY=... go test ./internal/llm -run TestLive -count=1 -v
```

With a DeepSeek key, a second test makes one structured call with a 256-token
output ceiling, which a thinking model spends on reasoning before it calls a
tool. It checks that the error names the `length` finish and suggests raising
`max_output_tokens`, and skips if the model answers within the ceiling.

The Anthropic test also sends the `thinking-binding-controls-2026-08-01` beta
header without changing the mismatch behavior, so accounts that do not enforce
thinking-block binding still report a replay whose prefix changed; the test
fails on any such report. Tests read these variables before clearing the
ambient environment, so other tests never see them. Never commit keys or paste
them into logs.

Before opening a pull request, also check that `go mod tidy` leaves `go.mod`
and `go.sum` unchanged, including the tool modules (run `go mod tidy` inside each
tool directory). CI runs vulnerability, Git-history secret, and
dependency-license checks. New runtime dependencies must use a license
compatible with Roast's MIT/Apache-2.0 dual license and should be justified in
the pull request.

Pull-request CI runs on GitHub-hosted runners with read-only repository
permissions and no inference, upload, or tap credentials. The `claude-review`
and `claude-mention` workflows are the exception: they hold an Agent Gateway
inference credential and can comment on pull requests, but review only
non-draft PRs from branches in this repository and answer only commenters with
write access. Keep that boundary: do not run contributor-controlled code in
`pull_request_target` or a privileged `workflow_run` job. Pin external actions
to full commit SHAs. Go caches use the standard setup-go branch scopes, and
container caches use BuildKit's GitHub Actions backend. Do not bootstrap
privileged jobs from pull-request artifacts.

Keep changes narrow and include regression tests for behavior changes. Update
the README whenever a public flag, environment variable, configuration field,
artifact, data destination, or exit-code behavior changes.

Pull request titles need a conventional `type:` prefix because squash merges
turn them into release-note entries; see upstream's [Contributing](https://github.com/bluesky-social/roast#contributing) section.
Branch protection requires the `Test and build` and `Security and licenses`
CI checks and the PR-title check to pass, and requires a pull request to be up
to date with `main` before it merges.

## Preview releases

Maintainers can manually run the `Preview Release` GitHub Actions workflow for
the revision selected in the workflow UI. It creates a GitHub prerelease with
a non-production tag and a `roast-preview` executable, without updating
Homebrew or the stable release channel. Preview visibility follows repository
visibility.

Download and unpack the archive for your platform with `gh release download`,
then run `roast-preview`. It reads the normal Roast configuration but can live
beside an installed `roast`. To exercise a pre-commit gate with the preview
binary, set `ROAST_BIN=/absolute/path/to/roast-preview` for the Git operation.

## Stable releases and Homebrew

Release Please maintains a release pull request on `main`. Merging that pull
request creates the stable tag and a **draft** release. GoReleaser reuses that
draft, preserves Release Please's notes, and publishes it only after uploading
the release artifacts. An unfinished draft is not returned by GitHub's latest
release API, so Roast does not advertise an upgrade before downloads are ready.
GoReleaser updates `Casks/roast.rb` on `main` in
`bluesky-social/homebrew-tap` with this repository's asset URLs and checksums.
Existing `bluesky-social/tap` installations keep the same package name and
upgrade with `brew upgrade --cask bluesky-social/tap/roast`, or with a plain
`brew upgrade` once the tap is trusted (`brew trust --tap bluesky-social/tap`;
Homebrew 7 and later).

The `release-please` workflow runs on every push to `main` and hourly during
the weekday release window, keeping a standing `chore(main): release X.Y.Z`
pull request up to date. A GitHub App rather than `GITHUB_TOKEN` maintains that
Release PR, so its CI runs normally. A Release PR auto-merges only when every
commit since the last tag came from a merged Renovate or Dependabot PR made of
that bot's own verified commits, and only from the hourly scheduled run on
weekdays 09:00-16:00 PT. Required CI checks and strict branch protection still
apply. Any human-authored change, including a `fix:` or a commit that would
not release on its own, waits for a maintainer to merge the Release PR.
Renovate supplies the dependency aging policy (three days for ordinary
updates, no delay for advisory-driven updates). Maintainers can run the
workflow manually to queue an eligible release immediately or reconcile a
missed tag. Once a release is cut, PR-maintenance and gate failures in the
same run do not block the binaries job.

Renovate opens dependency pull requests according to `renovate.json`. Do not
push to a Renovate PR or use "Update branch" on one: either adds a
human-authored commit, so the next release waits for a maintainer. If a
dependency update needs code changes, make them in a separate pull request.

If release building or uploading fails, fix the cause and use GitHub's **Re-run
failed jobs** on the original run. That retains the original release outputs
and reuses the draft. Once the release is published, draft lookup is no longer
the right retry mode. To repair a failed tap update, rebuild the same tag with
GoReleaser's default `release.use_existing_draft: false` in a temporary local
configuration, preserving `release.mode: keep-existing`. Do not create or retag
a new version just to repair an upload, or commit that retry-only configuration.

To hold a patch release, add the `release:manual` label and disable any already
queued auto-merge (`gh pr merge <number> --disable-auto`). Automation leaves
that PR for manual review. Removing the label makes it eligible again.

The release workflow publishes the cask with a short-lived installation token
from the public release GitHub App (`PUBLIC_RELEASE_APP_ID` and
`PUBLIC_RELEASE_APP_KEY`), minted per run and scoped to
`bluesky-social/homebrew-tap` with contents write. The App must be installed on
the tap repository. The workflow mints and checks the token before building a
stable release. Preview releases never update the tap.

Validate packaging locally with the GoReleaser version pinned in the workflows:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Snapshot builds generate `roast-preview` archives and checksums without
publishing. The preview workflow uses this same packaging configuration, never
updates Homebrew, and publishes only a prerelease. Stable builds retain the
`roast` executable and cask identity. Before a stable release, check its setup
caveats and test the generated cask's download strategy. Private assets use an
authenticated `gh` fallback; public assets download normally without requiring
`gh`.

Build metadata, binary modification times, and archived file timestamps derive
from the commit, not the build clock. To compare rebuilt archives, use the same
commit, configuration, toolchain, and snapshot version (set
`ROAST_PREVIEW_VERSION=preview-local` for a fixed preview version).

Homebrew upgrades replace a manually linked preview at Homebrew's `roast`
path. Run `roast --version` after upgrading to confirm which binary is active.

## Dependency notices

`bash scripts/licenses.sh` checks and collects runtime dependency licenses and
notices for all six CGO-disabled CLI release targets, plus the broker. It uses
the `go-licenses/v2` tool module in `.github/tools/licenses`, generates per-target
CSV inventories, and includes the Go runtime/standard-library license.
It also extracts the embedded Igneous Systems MIT grant from the selected
`md5-simd` assembly source into `NOTICE-Igneous-Systems-MIT.txt`; the scanner only
collects its root Apache license. Generation fails if the expected complete
grant cannot be found, leaving any previous generated bundle intact.
Likewise, `golang.org/x/net/publicsuffix` embeds MPL-2.0 Public Suffix List
data that x/net's BSD license does not cover. The script writes
`NOTICE-publicsuffix-MPL-2.0.txt` beside x/net's license with the list revision
named in the selected module source, where to obtain that revision, and the
verbatim license text from `scripts/notices/MPL-2.0.txt`. It fails if the
revision cannot be found.
Generated files live in the ignored
`THIRD_PARTY_LICENSES/` directory, separate from Roast's own licenses. The script
refuses to replace a directory it did not generate.

The ownership marker identifies the bundle format, not the scanner version, so
tool upgrades do not invalidate a previously generated bundle.

GoReleaser runs this script before packaging, preview releases use the same
bundle, and CI checks generation for every target. The broker Dockerfile
generates notices for its Linux architecture and puts them, with Roast's own
licenses, under `/usr/share/licenses/roast/` in the final image. When changing
dependencies, inspect the generated inventories and any non-Go-code inspection
warnings; automated classification is not a substitute for legal review.

Prefer tagged stable dependency releases. Explain any necessary development pin
or prerelease in its pull request and revisit it when a stable release permits.
Rerun provider tests, module checks, vulnerability checks, and notice generation
after dependency changes; old advisory notes are not a substitute for scanning
the selected module graph.

## Security

Do not report vulnerabilities in a public issue. Follow [SECURITY.md](SECURITY.md)
instead.

## Direct ChatGPT plan backend checks

The fork's `chatgpt` tests use mocked OAuth/JWKS and local stream fixtures. They
must not discover credentials or start authorization as part of normal tests.
The opt-in live test uses only the independent account created by
`roast chatgpt signin`, sends a fixed diagnostic prompt and random tool token,
and never sends source files:

```sh
ROAST_LIVE_CHATGPT=1 ROAST_LIVE_CHATGPT_MODELS=gpt-6.1-sol,gpt-6-astra \
  go test ./internal/llm -run TestLiveChatGPTTwoTurnReview -count=1 -v
```

Choose account-available model slugs. This consumes ChatGPT plan usage and has
request deadlines but no hard generation-token ceiling, as required by the
preview. Never enable it without the account owner's authorization. Mock tests
or an existing Codex CLI login are not evidence of live direct-route success.
