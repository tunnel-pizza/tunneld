# Security Policy

## Supported Versions

`tunneld` is pre-1.0 (`0.x`). Only the latest tagged release receives
security fixes. Once `1.x` ships, the support window will move to "latest
minor" of the current major.

| Version | Supported          |
| ------- | ------------------ |
| latest `0.x` | :white_check_mark: |
| older `0.x`  | :x:                |

## Reporting a Vulnerability

Please report security vulnerabilities **privately** via GitHub's
[private vulnerability reporting](https://github.com/tunnel-pizza/tunneld/security/advisories/new)
on the Security tab. That opens a draft advisory only the maintainers can see.

Please do **not** open a public issue for a suspected vulnerability.

### What to include

- A clear description of the issue and its impact.
- Steps to reproduce (a minimal example, version/commit, OS).
- Whether the issue is exploitable with default configuration.
- Any suggested fix or mitigation, if you have one.

### Expectations

- Acknowledgement within 7 days.
- A status update within 30 days, including a plan and rough timeline.
- A coordinated disclosure once a fix or workaround is available; we will
  credit you in the advisory unless you ask otherwise.

## Verifying releases

Every asset on a tagged release is signed with
[cosign](https://github.com/sigstore/cosign) in keyless mode (Sigstore
Fulcio cert, Rekor transparency log), each with a self-contained signature
bundle beside it:

- the six binaries, `tunneld-<platform>-<arch>` (`.exe` on Windows), each
  with a `.sigstore` bundle;
- `checksums.txt`, the sha256 of each binary, with its own bundle — verify
  that once and `sha256sum -c` covers the rest;
- `vX.Y.Z.tar.gz.sigstore` and `vX.Y.Z.zip.sigstore`, bundles for the source
  archives GitHub serves for the tag.

The Windows executables are signed this way too — cosign, not Authenticode —
so SmartScreen will still ask; the bundle is what says the bytes are ours.

To verify a binary (cosign v2+):

```sh
TAG=v0.1.0
REPO=tunnel-pizza/tunneld
BIN=tunneld-linux-x64

for f in "$BIN" "$BIN.sigstore"; do
  curl -fsSL "https://github.com/${REPO}/releases/download/${TAG}/${f}" -o "$f"
done

cosign verify-blob \
  --bundle "${BIN}.sigstore" \
  --certificate-identity-regexp '^https://github.com/tunnel-pizza/tunneld/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$BIN"
```

A source archive verifies the same way, against the archive GitHub serves:

```sh
TAG=v0.1.0
REPO=tunnel-pizza/tunneld

curl -fsSL "https://github.com/${REPO}/archive/refs/tags/${TAG}.tar.gz" \
  -o "${TAG}.tar.gz"
curl -fsSL "https://github.com/${REPO}/releases/download/${TAG}/${TAG}.tar.gz.sigstore" \
  -o "${TAG}.tar.gz.sigstore"

cosign verify-blob \
  --bundle "${TAG}.tar.gz.sigstore" \
  --certificate-identity-regexp '^https://github.com/tunnel-pizza/tunneld/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${TAG}.tar.gz"
```

`Verified OK` means the bytes match what the release workflow signed.

## Scope

In-scope: anything in this repository's library code (the root `tunneld`
façade, `v1/`, `v1alpha1/`) or its release artifacts.

Out of scope: vulnerabilities in third-party dependencies or the Go
standard library itself (report those to their respective projects), and
issues that require an attacker to already have local execution as the
same user.
