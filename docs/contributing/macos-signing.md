# Signing and notarizing the macOS binaries

The release signs every darwin binary it builds, and notarizes them when the
App Store Connect key is configured (#2084). This page is for maintainers: what
the pipeline does, the five repository secrets it reads, and how to create
them.

## What the release does

GoReleaser runs `hack/macos-sign.sh` as a post-build hook for each darwin build,
on the Linux release runner, before anything is archived:

| Build                  | Binary                | Identifier                  | Info.plist template                              |
|------------------------|-----------------------|-----------------------------|--------------------------------------------------|
| `llmkube-cli`          | `llmkube`             | `dev.llmkube.cli`           | none (a Terminal tool needs no bundle identity)  |
| `llmkube-metal-agent`  | `llmkube-metal-agent` | `dev.llmkube.metal-agent`   | `deployment/macos/metal-agent.Info.plist.tmpl`   |
| `llmkube-foreman-agent`| `foreman-agent`       | `dev.llmkube.foreman-agent` | `deployment/macos/foreman-agent.Info.plist.tmpl` |

1. The template is rendered with the release version and embedded as a
   `__TEXT,__info_plist` section by `cmd/machoplist`. Apple's linker would do
   this with `-sectcreate`, which Go only reaches through cgo and a darwin
   linker; the release builds with `CGO_ENABLED=0` on Linux, so the tool edits
   the finished Mach-O instead (`internal/machoplist`).
2. [rcodesign](https://github.com/indygreg/apple-platform-rs) signs the
   binary with the identifier above and binds the embedded plist. The release
   job installs rcodesign 0.29.0, pinned by version and SHA-256, in
   `.github/workflows/release-please.yml`.
3. With the notary key set, the binary is zipped, submitted to Apple's notary
   service, and the hook waits up to 20 minutes for the verdict. A rejection
   fails the release, so a signed but rejected binary never ships.

GoReleaser's own `notarize.macos` step is deliberately not used: it signs with
the file name as the identifier and does not bind the plist.

What the secrets select:

| Secrets set                          | Result                                                              |
|--------------------------------------|---------------------------------------------------------------------|
| none                                 | ad-hoc signature (identifier and plist bound), not notarized        |
| `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD` | Developer ID signature, hardened runtime, secure timestamp       |
| all five                             | Developer ID signature, notarized                                   |
| any other combination                | the release fails with the missing variable named                   |

The ad-hoc fallback keeps forks and secret-less runs working: embedding the
plist breaks the Go linker's own ad-hoc signature, and macOS kills an arm64
binary whose signature does not verify, so the binary is re-signed either way.

## The secrets

Set them on `defilantech/LLMKube` (ideally in a protected environment used only
by the release job). All are read from the GoReleaser step's environment.

| Secret                   | Value                                                         |
|--------------------------|---------------------------------------------------------------|
| `MACOS_SIGN_P12`         | base64 of the Developer ID Application certificate and key (`.p12`) |
| `MACOS_SIGN_PASSWORD`    | the `.p12` export password (required; it cannot be empty)     |
| `MACOS_NOTARY_KEY`       | base64 of the App Store Connect API key (`AuthKey_<KEYID>.p8`) |
| `MACOS_NOTARY_KEY_ID`    | the key's ID                                                  |
| `MACOS_NOTARY_ISSUER_ID` | the issuer ID shown above the keys list                       |

### Developer ID Application certificate

1. Create a Developer ID Application certificate in Certificates,
   Identifiers & Profiles (Apple reserves this for the account holder unless
   the account grants admins access). Consider a separate certificate for CI,
   so revoking it does not affect local signing.
2. In Keychain Access, find "Developer ID Application: <team>" under My
   Certificates, expand it to confirm the private key is there, and export
   both as a `.p12` with a strong password.
3. Store it, then delete the file:

   ```bash
   base64 -i developer-id.p12 | gh secret set MACOS_SIGN_P12 --repo defilantech/LLMKube
   gh secret set MACOS_SIGN_PASSWORD --repo defilantech/LLMKube   # prompts for the value
   rm developer-id.p12
   ```

### App Store Connect API key

1. In App Store Connect, Users and Access, Integrations, App Store Connect API,
   create a Team Key with the Developer role (enough for notarization).
2. Download `AuthKey_<KEYID>.p8`; Apple offers the download only once. Note the
   key ID and the issuer ID.
3. Store them, then delete the key file:

   ```bash
   base64 -i AuthKey_<KEYID>.p8 | gh secret set MACOS_NOTARY_KEY --repo defilantech/LLMKube
   gh secret set MACOS_NOTARY_KEY_ID --repo defilantech/LLMKube --body <KEYID>
   gh secret set MACOS_NOTARY_ISSUER_ID --repo defilantech/LLMKube --body <issuer-id>
   rm AuthKey_<KEYID>.p8
   ```

The hook decodes the secrets into a private temporary directory, passes them to
rcodesign as files, never prints them, and removes the directory when it exits.

## Stapling and Gatekeeper

The release ships bare executables, and a notarization ticket can only be
stapled to an app bundle, disk image or installer package. Gatekeeper therefore
looks the ticket up online the first time a quarantined (downloaded) copy runs.
A notarized binary checks as:

```bash
spctl --assess --type install -vv llmkube-metal-agent   # accepted, source=Notarized Developer ID
codesign -dv --verbose=4 llmkube-metal-agent            # Identifier=dev.llmkube.metal-agent, Info.plist entries=9
```

## Local builds

- `make build-metal-agent` and `make install-metal-agent` do not sign or embed
  anything; the binary keeps the Go linker's ad-hoc signature.
- A GoReleaser snapshot runs the same hook:
  `go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=publish,docker,sbom`.
  With `rcodesign` on `PATH` and no secrets, the darwin binaries get the plist
  and an ad-hoc signature; without it the hook prints a warning and leaves them
  as built.
- `make test-scripts` runs `scripts/macos-sign_test.sh`, which checks the hook's
  secret handling with a stubbed rcodesign and, on macOS with rcodesign
  installed, signs a real binary.

## Changing things

- The identifiers are the binaries' identity to macOS privacy settings; keep
  them stable. They must match in `.goreleaser.yaml`, the Info.plist templates
  and the `AssociatedBundleIdentifiers` of the launchd plists in
  `deployment/macos/`; `internal/machoplist/release_identity_test.go` checks
  that they do.
- To update rcodesign, change the version and the SHA-256 of the
  `x86_64-unknown-linux-musl` tarball in the "Install rcodesign" step.
- A Developer ID certificate is valid for five years. After renewing it,
  export and store the new `.p12` the same way.
