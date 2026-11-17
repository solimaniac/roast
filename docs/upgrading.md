# Upgrading from pre-1.1 configurations

Roast 1.1 introduced a versioned, provider-neutral configuration format
(`version: 1`). This guide moves an installation configured for an earlier
release to it. The review command, durable local runs, the optional Git
pre-commit gate, and opt-in run uploads carry over. External retrieval,
telemetry, and the Claude PostToolUse findings hook were removed, and replay
and evaluation are not part of the `roast` command.

## 1. Install the current release

Upgrade with the method you originally used to install Roast (see the
README's [Install](../README.md#install) section), then confirm which binary is
active:

```sh
roast --version
```

## 2. Initialize configuration

If your organization provides a trusted configuration URL, run:

```sh
roast config init --from https://config.example/roast
roast doctor
```

Use the URL your organization provides, not the illustrative URL above.
An interactive `roast review` or `roast runs sync` (stdin and stderr are
terminals) also offers the configuration-URL prompt in three cases: the default
config file is missing and the finder and verifier cannot be resolved from the
environment or flags; the file is unversioned; or the file has Claude profiles
using Chat Completions or Responses. The last two cases prompt even when the
roles already resolve. Compatibility protocols can be intentional; leave the
URL blank to keep them. For versioned configs, that choice is recorded in a
private sibling `.protocol-update-ack` file containing only the config's hash,
so the prompt does not repeat unless the config changes. Explicit `config init` still works after
a decline. Non-interactive runs do not prompt or update files. An explicit
`--config` bypasses automatic enrollment.

The configuration document must declare `version: 1` and contain no inline
secrets. It may name credential environment variables. If a trusted template
explicitly opts into `provisioning.import_legacy_credential: true`, Roast carries
the old `gateway.api_key` into otherwise credential-free inference profiles and
presigned-broker authentication locally. It never sends that credential to the
configuration URL. A template which names credential environment variables does
not have those sources replaced by the legacy key.

Releases before 1.1 read `$XDG_CONFIG_HOME/roast/config.yaml`, or
`~/.config/roast/config.yaml` when `XDG_CONFIG_HOME` is unset, on every
platform. Current releases use the operating system's user configuration
directory by default, but also honor an explicitly set `XDG_CONFIG_HOME` on
macOS. The README's [Config file location](../README.md#config-file-location)
section lists the paths and overrides. On macOS, set `XDG_CONFIG_HOME` to
`$HOME/.config` to keep using the old path. Otherwise, on macOS and Windows the
old and new paths differ, and Roast consults the old one only while installing
from a configuration URL; without a URL, move the file to the new path or pass
it with `--config`.

On macOS, an existing non-empty `XDG_CONFIG_HOME` changes the selected path after
this upgrade, even if you did not change your environment. Roast does not fall
back to `~/Library/Application Support/roast/config.yaml`. If the selected file
is missing, Roast uses defaults plus environment variables. Non-interactive
commands do not warn about the previous file.

If you already export an absolute `XDG_CONFIG_HOME`, move your configuration from
`~/Library/Application Support/roast/config.yaml` to `$XDG_CONFIG_HOME/roast/config.yaml`.
To keep the file at its existing location, pass
`--config "$HOME/Library/Application Support/roast/config.yaml"`.
Run `roast doctor` to check the selected path before you run reviews, hooks, or agents.

A relative `XDG_CONFIG_HOME` on macOS was previously ignored. After this upgrade,
commands that use the default path fail with a relative value. Use an absolute
path, unset the variable, or pass an explicit `--config`.

`roast doctor` shows the selected config path. In-place legacy replacement keeps
a `.pre-v1` backup and writes the new file with owner-only permissions on Unix.
When the destination differs, the old file is left untouched. Both the old file
and any backup can contain credentials: protect them and never attach them to a
public bug report.

For an existing versioned destination, `config init` only repairs Claude
compatibility protocols authorized by the supplied template. The template must
configure a Claude profile with `protocol: anthropic` at the same endpoint.
Matching local Claude profiles change to native Messages, including alternate
profiles selected by author routes. Model names, credentials, output limits,
roles, prompts, uploads, and other local settings remain unchanged. YAML comments
are preserved, and the original file is backed up as `.pre-protocol-update` with
owner-only permissions. Aliases which would change unrelated settings cause the
update to stop without replacing the file.

The template does not replace other settings in a versioned destination. If it
authorizes no matching protocol updates, the existing file is left untouched.
For a deliberate new configuration, use `--output <new-path>`. Updating a remote
template alone does not update existing installations: users must accept the
interactive URL prompt or run `config init` explicitly. No organization-specific
endpoint or model policy is built into Roast.

Without a configuration URL, use the README's named-profile examples. Roast
can load old inline-role configs in memory; the retired `gate` and `sourcebot`
sections are recognized and discarded rather than restoring those features.
That compatibility does not discover a replacement upload endpoint. Old
`upload.enabled: true` alone never enables uploads to a new destination.

## 3. Review changed configuration and environment settings

| Old setting | New behavior |
| --- | --- |
| Inline role `model` / `api` / `author_flip` | Named `inference_profiles` with `model` / `protocol` / `endpoint`; roles select profiles and `author_routes` select complete alternative profiles. |
| Global `ROAST_API_KEY` / `AIGW_API_KEY` | Not used as global overrides. Use profile `api_key_env`, a user-config `api_key`, or role-specific `ROAST_FINDER_API_KEY` / `ROAST_VERIFIER_API_KEY`. |
| `gate.auto_install` / `ROAST_HOOK_INSTALL` | Retired. The first interactive review asks once per repository; a decline is recorded in local Git config. `ROAST_NO_HOOK=1` suppresses bootstrap, and `roast gate enable|status|disable` manages explicit enrollment. |
| Old upload enrollment | Configure an S3-compatible destination with explicit credential environment variables, or a presigned broker with `token_env`/locally imported token. Uploads remain disabled unless the new config explicitly enables them. |

Provider variables are a fallback: `OPENAI_API_KEY` / `OPENAI_BASE_URL` serve
`chat` and `responses`, and `ANTHROPIC_API_KEY` / `ANTHROPIC_BASE_URL` serve
`anthropic`. A provider `*_BASE_URL` applies only to a profile that sets no
`endpoint`. A provider `*_API_KEY` from the shell is sent only to that
provider's default endpoint or to the endpoint named by the same provider's
`*_BASE_URL`. A profile with any other endpoint, such as a gateway, must name
its credential with `api_key_env` (or a user-config `api_key`), or the role
needs `ROAST_FINDER_API_KEY` / `ROAST_VERIFIER_API_KEY`. A gateway profile that
relied on an ambient provider key otherwise fails with "API key is not
configured". Role-specific settings override provider variables. Remove
obsolete environment exports when you have moved to explicit credential
sources.

## 4. Refresh the skill and check the gate

```sh
roast skill install
roast skill status
roast gate status
```

Run the gate command inside each repository you care about. Keep your native
Git gate; it is separate from the retired Claude hook. Enable it explicitly with
`roast gate enable`, or accept the one-time interactive prompt. A declined
prompt does not prevent later explicit enrollment. Non-interactive invocation
does not install a hook without consent.

If `core.hooksPath` is shared, each repository must opt in independently.
`roast gate enable` refreshes an older Roast-owned dispatcher; its presence in
another repository no longer enrolls that repository. Disabling a custom hooks
path leaves its dispatcher in place for other repositories and records only the
current repository's decline. Check each repository's enrollment with
`roast gate status`.

## 5. Optional cleanup of the retired Claude integration

This is cleanup on an existing user's machine, not an integration shipped by
current releases. `roast skill install` deliberately does not edit Claude
settings or delete previously installed hooks.

If you previously used the pre-1.1 `roast install`:

1. Locate your Claude config root, normally `~/.claude`, or the directory named
   by `CLAUDE_CONFIG_DIR`. Make a separate, non-overwriting backup of its
   `settings.json` before editing it.
2. Under `hooks.PostToolUse`, remove only command entries which invoke the old
   Roast-owned `hooks/roast/posttooluse-roast.sh`. Older installations may point
   to a different location for that same script; inspect the command before
   removing it. Preserve other commands in the same matcher and all unrelated
   hook events. Do not remove the native Git pre-commit gate.
3. Under `footerLinksRegexes`, remove the old Roast report-link entries: labels
   `R-{alias}` and `roast {id}`, with `vscode://file/{path}` URLs and patterns
   matching Roast's `runs/<run-id>/report.md` paths. Preserve unrelated entries.
4. Once no settings reference it, the old Roast-owned script may be removed.
   Remove only that script, not the Claude hooks directory. No bulk deletion or
   automatic settings rewrite is required.

If those entries or files do not exist, there is nothing to clean up. Restart
Claude after editing settings and confirm `roast skill status` still reports
the installed skill.

## 6. Verify a review and, if enabled, uploads

Run `roast doctor`, then review a small non-empty change. `doctor` sends a small
inference probe to each configured role; a review sends the selected repository
material to those endpoints. Confirm they are trusted before using private code.

When the new config explicitly enables uploads, the completed run should upload
automatically. `roast runs status` shows its state; `roast runs sync` retries
pending uploads. Upload failures do not change the review's result or exit
status. Do not use `--include-existing` unless you intend to upload older local
run records as well.

For issues, share the Roast version and redacted error text. Do not share config
credentials or complete run bundles without checking their contents; bundles
can include proprietary code, prompts, snapshots, and model responses.
