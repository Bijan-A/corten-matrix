# Dev notes

## Filing GitHub issues / sharing debug output

**Never paste real contact data into an issue, PR, or commit** — phone numbers,
email addresses, Apple IDs, or iMessage handles belonging to an actual person
(including your own). This applies to pasted logs, config file excerpts,
screenshots, and crash reports.

Before filing an issue or attaching debug output:
- Scrub `pkg/imconfig`-style YAML/JSON of real `+1XXXXXXXXXX` phone numbers,
  `user@domain` emails, or Apple ID handles — replace with placeholders like
  `+15555550100` / `user@example.com`.
- Redact device/session/push tokens the same way as credentials — treat them
  as secrets even though they aren't API keys.
- Prefer reproducing the bug with test/throwaway account data over your own
  real contacts when possible.

If real personal data does end up in a filed issue: edit it immediately to
redact it, and flag it to a maintainer — GitHub keeps edit history on issues,
so a plain edit isn't enough; the sensitive revisions need to be purged
separately (via GitHub support or admin-level history deletion).

## FFI boundary (Go ↔ Rust)

**Never hand-edit** `pkg/rustpushgo/rustpushgo.go` or `rustpushgo.h`. Always regenerate.

### macOS

```bash
make bindings   # requires uniffi-bindgen-go on PATH
make build
```

macOS only — see [the README](README.md#linux-is-not-supported-here) for why Linux cannot
be built from this tree.

Install `uniffi-bindgen-go` (must match UniFFI 0.25.0 as pinned in `pkg/rustpushgo/Cargo.toml`):
```bash
cargo install uniffi-bindgen-go --git https://github.com/NordSecurity/uniffi-bindgen-go --tag v0.2.2+v0.25.0
```

### Windows

Not supported. The helper scripts this section used to describe (`dev\windows-dev-env.bat`,
`dev\windows-bindings.bat`) are **not present in this tree**, and the Makefile hard-errors on
any non-Darwin host anyway:

```
This bridge builds on macOS only: NAC uses Apple's native AAAbsintheContext framework.
```

Regenerate bindings on macOS with `make bindings`.

## Config

### Network config (the `network:` / iMessage settings)

**Source of truth: `pkg/imconfig/example-config.yaml`** — the documented
defaults and comments users see. It is embedded **verbatim** via `//go:embed`
into `NetworkExampleConfig` (`pkg/imconfig/imconfig.go`); it is **not** generated
from Go, and there is no separate template. Both `pkg/connector` and `cmd/bbctl`
consume this one embedded copy, so **never re-add a hardcoded config string**
elsewhere — the two generation paths drifted once, which is exactly why this was
unified into `pkg/imconfig`.

The matching Go struct is `IMConfig` in `pkg/connector/config.go`, with
`upgradeConfig` handling migrations.

To add or change a network option, edit all three in lockstep:
1. `pkg/imconfig/example-config.yaml` — the YAML key, default value, and the
   user-facing comment.
2. `pkg/connector/config.go` — the `IMConfig` struct field (+ `yaml:` tag) and
   its doc comment. Keep this comment's wording in sync with the YAML comment.
3. `pkg/connector/config.go` `upgradeConfig` — add a `helper.Copy(...)` line so
   the option survives a config upgrade.

`pkg/imconfig/imconfig_test.go` guards that the embedded YAML stays valid YAML.

### Bridge (base/appservice) config — the `bridge:` section

This is the **bridgev2 framework's** config (`bridgeconfig.BridgeConfig`), not
ours. To change a framework default, override it in `IMConnector.Start()`
(config YAML is loaded by then) — see the existing overrides there for
`phone_numbers_in_profile` (must be true or bridgev2 strips `tel:` from contact
profiles → no call button), `unknown_error_auto_reconnect`, and
`unknown_error_max_auto_reconnects`.
