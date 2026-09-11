# Releasing scruff

Six artifacts out of one repository — the CLI plus five SDKs — all carrying the
same version number. One tag publishes all of them.

| artifact | published as | how |
|---|---|---|
| CLI | the GitHub release + source tarball | Nix consumers take the flake input; there is no binary to attach |
| `sdk/ts` | npm `@hausfold/scruff` | `npm publish` over OIDC |
| `sdk/python` | PyPI `hausfold-scruff` | `gh-action-pypi-publish` over OIDC |
| `sdk/rust` | crates.io `hausfold-scruff` | `cargo publish` over OIDC |
| `sdk/go` | `github.com/hausfold/scruff/sdk/go` | a `sdk/go/v<version>` tag — Go's proxy needs nothing else |
| `sdk/swift` | `github.com/hausfold/scruff-swift` | a `<version>` tag on the mirror — SwiftPM likewise |

## Cutting one

```sh
bench release scruff <X.Y.Z>
```

That is the whole flow: it stamps the version into every manifest, commits,
pushes, tags `v<X.Y.Z>`, then blocks — painting the CI job tree live — until
every publish job finishes, and exits non-zero if one goes red.

Never push a `v*` tag by hand. The `version stamp` job in
[`.github/workflows/release.yml`](../.github/workflows/release.yml) re-checks
every manifest against the tag and fails the run if they disagree.

## The number

Semver, a plain `X.Y.Z` with no prerelease suffix. npm, PyPI and crates.io
already hold semver versions under the pre-rename `holt` names and never let a
published version be withdrawn — only superseded — so the number is a
compatibility contract people pinned against, not a date. CalVer would also
force the Go SDK's import path to end in `/v2026` and change it every January.
The suffix is barred because PEP 440 would rewrite `0.2.0-rc1` to `0.2.0rc1` on
the Python side while npm and crates kept it verbatim, and the one number would
stop being one number.

Judge the bump against the **published SDK surface**, not the CLI internals:

```sh
git diff "$(git describe --tags --abbrev=0 --match 'v*')"..main -- sdk/
```

All five share the one number, so a break in the Rust client alone bumps all
five — five clients agreeing about one wire format is the invariant the `sdks`
and `swift-sdk` jobs in [`check.yml`](../.github/workflows/check.yml) exist to
protect. The taxonomy and the worked examples are the workshop's
[`/release` skill](https://github.com/hausfold/workshop/blob/main/.agents/skills/release/SKILL.md).

## Where the version lives

[`script/stamp-version.sh`](../script/stamp-version.sh) owns `VERSION`,
`sdk/ts/package.json`, `sdk/python/pyproject.toml` and `sdk/rust/Cargo.toml`,
and is the only thing that should write them. `sdk/go` and `sdk/swift` declare
no version at all — for both, the tag *is* the release.

```sh
script/stamp-version.sh <X.Y.Z>            # write it everywhere
script/stamp-version.sh --check <X.Y.Z>    # what CI runs against the pushed tag
```

## Bootstrapping a registry

Publishing authenticates by OIDC — except the Swift mirror, which pushes to
*another* repository and so needs the `MIRROR_TOKEN` PAT `release.yml`'s header
describes, the one credential here that can expire. That header also carries the
browser form each of the other three registries needs, and all three are wired.
What it doesn't say is what adding a *new* package costs, because a trusted
publisher matches on repo **and** package name — so a rename starts over, and
none of the old entries carries across:

- **npm and crates.io both insist the package exist first**, so a name neither
  has seen has to be published by hand once before CI can take it. PyPI's
  *pending* publisher (under your account, not the project sidebar) is the only
  one of the three you can wire before the tag.
- **The npm hand publish needs a passkey, and a CLI new enough to ask for one.**
  npm stopped enrolling TOTP authenticators, so it hits 2FA with no six-digit
  code to type. The hand-off that makes `npm publish` print an auth URL instead
  of dying on a bare `EOTP` landed in **npm CLI 11.9**; older just fails.
  `npx --yes npm@11.19.1 publish --access public` is enough (it wants node
  ≥22.9). `npm login --auth-type=web` first if the stored token is a narrow
  granular one — and login **overwrites** `_authToken` in `~/.npmrc`, so copy it
  aside.
- **crates.io's token must carry the `publish-new` scope**, and a crate-scoped
  token can't be made for a crate that doesn't exist yet — mint one with the
  crate-scope field empty.
- Leave *environment name* blank on every form. No job here declares an
  `environment:`, and a claim that names one never matches.

A name you leave behind stays published forever: deprecate it in place, never
yank — a yank breaks every consumer on the way out.

## When a publish fails

The publish jobs are deliberately independent and idempotent, so a registry that
rate-limits or isn't wired yet fails alone and a rerun finishes the release
rather than half-cutting a second one:

```sh
gh run rerun --failed --repo hausfold/scruff <run-id>
```

**Never respond to a failed publish by bumping the version** — that burns a
number permanently on the registries that did succeed.

## Afterwards

The version stamp is a commit, so scruff's HEAD moved and haus's `flake.lock`
pin of it is now stale:

```sh
bench ship                          # or: bench release scruff <X.Y.Z> --ship
```
