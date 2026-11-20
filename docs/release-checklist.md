# Release checklist

Work through this list, in order, before pushing a release tag, including `v1.0.0-rc.1`.
Release tags (`refs/tags/v*`) and adapter contract module tags
(`refs/tags/pkg/accelerator/v*`, `refs/tags/pkg/cloud/v*`) can be created,
moved, or deleted only by repository admins (repository ruleset "release
tags: admins only").

## 1. Gates

- [ ] `make gates` passes on the release commit.
- [ ] For rc.1, review the live CPU replacement results for EKS, GKE and AKS against the capability matrix. Record the synthetic accelerator capacity and the verification depth. Document any approved exception for a missing cloud run. Mark real GPU, TPU and Trainium validation separately.
- [ ] Replace any editorial HTML reminder in the README with verified results or remove it.
- [ ] Run govulncheck with the release toolchain on the final dependency graph. Review the pinned XZ decoder for advisories and for a maintained replacement with a hard dictionary limit.
- [ ] CI is green on `main` at the release commit.

## 2. Adapter contract modules (required before the next release)

`pkg/accelerator` and `pkg/cloud` are separate Go modules (the adapter
contract). Until they are tagged, the root `go.mod` requires them at a
placeholder version (`v0.0.0-00010101000000-000000000000`) through local
`replace` directives. Consumers of the root module ignore its replacements
and cannot resolve the placeholder. Versioned `go install` rejects the
root module's local replacements outright.

Before the first release that contains these modules (admin steps only;
prepare and review the code first, then tag the merged commit):

- [ ] Run `make verify-adapter-modules` and test each module standalone:

      ```sh
      (cd pkg/accelerator && GOWORK=off go test ./...)
      (cd pkg/cloud && GOWORK=off go test ./...)
      cmp pkg/accelerator/LICENSE LICENSE
      cmp pkg/cloud/LICENSE LICENSE
      ```

      Each module must contain the full Apache-2.0 text in a file named
      exactly `LICENSE` inside its module directory for pkg.go.dev. The dependency check covers tests as well as production
      code, rejects root/sibling modules, and uses Go's stdlib classification.
      It checks the current GOOS/GOARCH and build tags; run it for any new
      platform-specific adapter contract code too.

- [ ] On a clean, synced `main`, record the reviewed commit and create tags:

      ```sh
      git checkout main
      git pull --ff-only origin main
      test -z "$(git status --porcelain)"
      module_commit=$(git rev-parse HEAD)
      git tag -a pkg/accelerator/v0.1.0 "$module_commit" -m "Accelerator contract v0.1.0"
      git tag -a pkg/cloud/v0.1.0 "$module_commit" -m "Cloud contract v0.1.0"
      git push origin pkg/accelerator/v0.1.0 pkg/cloud/v0.1.0
      ```

      Do not move an existing published tag. These tags precede the root
      release: the root version update below is a separate commit.

- [ ] Verify public resolution outside the checkout (so local replacements
      cannot mask a broken publication):

      ```sh
      verify_dir=$(mktemp -d)
      (
        cd "$verify_dir"
        GOWORK=off GOPROXY=https://proxy.golang.org go list -m github.com/kubeneuron/kubeneuron/pkg/accelerator@v0.1.0
        GOWORK=off GOPROXY=https://proxy.golang.org go list -m github.com/kubeneuron/kubeneuron/pkg/cloud@v0.1.0
      )
      rmdir "$verify_dir"
      ```

      Both commands must print the module path and `v0.1.0`. If the proxy
      has not indexed the tags yet, retry before proceeding. Visit
      [accelerator v0.1.0](https://pkg.go.dev/github.com/kubeneuron/kubeneuron/pkg/accelerator@v0.1.0)
      and [cloud v0.1.0](https://pkg.go.dev/github.com/kubeneuron/kubeneuron/pkg/cloud@v0.1.0),
      request indexing if needed, and confirm the version, Apache-2.0 license,
      and rendered package documentation.

- [ ] In the root checkout, switch to real versions, **drop both replaces**,
      and commit the resulting `go.mod` and `go.sum`:

      ```sh
      go mod edit \
        -require=github.com/kubeneuron/kubeneuron/pkg/accelerator@v0.1.0 \
        -require=github.com/kubeneuron/kubeneuron/pkg/cloud@v0.1.0 \
        -dropreplace=github.com/kubeneuron/kubeneuron/pkg/accelerator \
        -dropreplace=github.com/kubeneuron/kubeneuron/pkg/cloud
      GOWORK=off go mod tidy
      make gates
      ```

      Recommendation: drop replacements. Versioned `go install` rejects
      modules containing these replacements, even when requires have real
      versions. This also makes release builds consume the published contract.
      For local contract development, use an untracked workspace instead:
      `go work init . ./pkg/accelerator ./pkg/cloud` (or `go work use` if a
      workspace already exists). Do not commit `go.work`/`go.work.sum`;
      use `GOWORK=off` for release verification.

- [ ] After committing the root version update, verify an isolated install
      from its public commit before tagging the root release:

      ```sh
      release_commit=$(git rev-parse HEAD)
      install_dir=$(mktemp -d)
      (
        cd "$install_dir"
        GOWORK=off GOPROXY=https://proxy.golang.org GOBIN="$install_dir/bin" \
          go install github.com/kubeneuron/kubeneuron/cmd/kubeneuronctl@"$release_commit"
      )
      ```

      This requires the reviewed commit to be public. Remove the temporary
      install directory after checking the binary. Do not publish the root
      release until resolution, install, and all release gates pass.

On later releases, tag a new `pkg/accelerator/vX.Y.Z` or `pkg/cloud/vX.Y.Z`
only when that module changed, and require it in the root `go.mod` before
tagging the release.

## 3. Tag and verify

- [ ] For `v1.0.0-rc.1`, verify that Chart.yaml has `version: 1.0.0-rc.1` and `appVersion: v1.0.0-rc.1`. Check the release workflow's `BASELINE` before tagging. With `v0.7.0`, confirm the rehearsal inputs are available. For a first release with no base, explicitly set `BASELINE: ''` and confirm the gate summary reports `upgrade rehearsal: skipped, no base release`. rc.1 supports a clean installation.
- [ ] Confirm approval before pushing the tag. **Pushing the tag starts public distribution**: the workflow can publish four tagged GHCR images before it creates the GitHub Release draft. For rc.1 the image tag is exactly `v1.0.0-rc.1`.
- [ ] Wait for `release-verify` to succeed for the draft ID created by this run. Review the install manifest, installer, CLI archives, image digests, SBOMs and signed checksums.
- [ ] For rc.1, confirm that exact release ID has the expected tag, `draft=true` and `prerelease=true`. After maintainer review, publish the verified draft by ID with `draft=false`, `prerelease=true` and `make_latest="false"`. Verify the published release retains prerelease status and the Latest release pointer has not moved. For stable tags the workflow publishes automatically after verification.
- [ ] Run `bash hack/verify-release.sh v1.0.0-rc.1 kubeneuron/kubeneuron` against the published candidate.

## Pinned inputs and tool versions

Every external build and test input is immutable: GitHub Actions by full commit
SHA (the version in a trailing comment), container images by multi-arch
digest (`name:tag@sha256:...`). `make lint` and CI run `hack/verify-pins.sh`,
which fails on an unpinned `uses:` or an image tag without a digest in
`.github/workflows/`, `build/Dockerfile` and `hack/*.sh`.

To update a pin:

- Action: resolve the tag to its commit
  (`gh api repos/OWNER/REPO/git/ref/tags/vX.Y.Z`, then the tag object's commit
  if it is an annotated tag), read the release notes, and replace the SHA and
  the version comment together.
- Image: `docker buildx imagetools inspect NAME:TAG` and copy the index
  `Digest:`; keep the tag in front of the digest so the version stays readable.
- Distroless base image: after changing its digest, run
  `python3 -I hack/base-image-packages.py` and commit
  `build/licenses/base-image-packages.txt` (the packages, licences and Debian
  sources each image ships as `/licenses/BASE-IMAGE-PACKAGES`). `make lint`
  fails while the record names other digests; `make verify-image` fails while
  its content is stale.
- Base-image sources, at every release: download the Debian source packages
  (`.dsc` and the archives it lists, from the snapshot.debian.org links in
  `build/licenses/base-image-packages.txt`) of the GPL and LGPL packages in the
  image (base-files, netbase, ca-certificates, glibc) and keep them in the
  release's private archive for three years. They back the source offer in
  `/licenses/BASE-IMAGE-PACKAGES`; do not attach them to the GitHub release.

Linters are pinned so a local `make lint` and CI agree: golangci-lint
v2.14.0 (CI `golangci-lint-action` `version:`), shellcheck v0.11.0 (CI installs
that release by checksum; actionlint uses it too).

Release verification (`hack/verify-release.sh`) requires cosign: it verifies
the checksum file's signature and every image digest in `images.txt` against
the release workflow identity
`https://github.com/kubeneuron/kubeneuron/.github/workflows/release.yaml@refs/tags/<tag>`.
Images are built with `--provenance=false` (buildx attestation manifests break
`kind load docker-image`); the cosign signatures and per-image SBOMs are the
supply-chain evidence, and the release makes no provenance claim.

## Native component source retention

The one native component KubeNeuron builds into the agent image is toybox
`nsenter` (0BSD), built from a checksum-pinned source and statically linked
against musl (MIT); its licence texts and source locations ship in
`/licenses/native/`. Neither licence asks for corresponding source, so
releases carry no native source archive. The distroless base image's Debian
packages keep their own licences.

Agent images up to v0.8.0 contained BusyBox (GPL-2.0) and uClibc-ng
(LGPL-2.1). Keep the `native-sources-busybox-*.tar.gz` assets of those
releases, and an owner-controlled backup of them, for at least three years
after the last distribution of any of those images, even after the images are
withdrawn. Record the last distribution date in the private release records.
