# Release checklist

Work through this list, in order, before pushing a release tag, including `v1.0.0-rc.2`.
Release tags (`refs/tags/v*`) and adapter contract module tags
(`refs/tags/pkg/accelerator/v*`, `refs/tags/pkg/cloud/v*`) can be created,
moved, or deleted only by repository admins (repository ruleset "release
tags: admins only").

## 1. Gates

- [ ] `make gates` passes on the release commit.
- [ ] For rc.1, review the live CPU replacement results for EKS, GKE and AKS against the capability matrix. Record the synthetic accelerator capacity and the verification depth. Document any approved exception for a missing cloud run. Mark real GPU, TPU and Trainium validation separately.
- [ ] For stable v1.0.0, require live runs on accelerator hardware and link their results in the capability matrix.
- [ ] Keep each README validation result marked `Pending` until run evidence supports replacing it.
- [ ] Replace any editorial HTML reminder in the README with verified results or remove it.
- [ ] Run govulncheck with the release toolchain on the final dependency graph. Review the pinned XZ decoder for advisories and for a maintained replacement with a hard dictionary limit.
- [ ] Run `make verify-licenses` on the release commit.
- [ ] Before tagging, dispatch the **Rehearse release image verification** workflow on the release branch and wait for a successful run. Confirm the run SHA matches the release commit.
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

- [ ] For rc.2, verify that Chart.yaml has `version: 1.0.0-rc.2` and `appVersion: v1.0.0-rc.2`, and that the default image tag in values.yaml is `v1.0.0-rc.2`.
- [ ] Check the release workflow's `BASELINE` before tagging. It names the previous published release: `v1.0.0-rc.1` for rc.2. Run `make test-upgrade-rollback-kind BASELINE=v1.0.0-rc.1` locally first. Confirm the `upgrade-rehearsal` job passes for both stores and does not report `upgrade rehearsal: skipped, no base release`.
- [ ] Dispatch the **Release preflight** workflow on the release commit with the version you will tag, for example `v1.0.0-rc.3`, and wait for a successful run. It runs the release's gates, source metadata check, upgrade rehearsal, image builds, install manifest and CLI archives, and publishes nothing. Confirm the commit in the run summary is the one you tag.
- [ ] Confirm approval before pushing the tag. **Pushing the tag starts public distribution**: the workflow can publish four tagged GHCR images before it creates the GitHub Release draft. For rc.2 the image tag is exactly `v1.0.0-rc.2`.
- [ ] Wait for `release-verify` to succeed for the draft ID created by this run. Review the install manifest, installer, CLI archives, image digests, SBOMs and signed checksums.
- [ ] Compare `/licenses/THIRD_PARTY_LICENSES` and `/licenses/NOTICE` from each signed image digest with `THIRD_PARTY_LICENSES` and `NOTICE` in the tagged tree. Require a byte-for-byte match.
- [ ] For rc.2, confirm that exact release ID has the expected tag, `draft=true` and `prerelease=true`. After maintainer review, publish the verified draft by ID with `draft=false`, `prerelease=true` and `make_latest="false"`. Verify the published release retains prerelease status and the Latest release pointer has not moved. For stable tags the workflow publishes automatically after verification.
- [ ] Run `bash hack/verify-release.sh v1.0.0-rc.2 kubeneuron/kubeneuron` against the published candidate.
- [ ] For the first stable (GA) release, return the install command in `docs/quickstart.md` and `docs/install.md` to `latest`, and review every "Not repeated on v1.0.0-rc.1" sentence in README and docs against the runs on the release being tagged.

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

For v1.0.0-rc.2 and later, confirm the draft includes `checksums-<tag>.txt.sigstore.json`
and verify it with Cosign 3.1.4 or later. Before publication, confirm the
`release-verify` job passed for the draft ID and verified the GitHub OIDC
signatures on both its checksums and image digests. For rc.1 use the
original signature and certificate as described in [Installation](install.md).
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

Agent images from v0.2.0 to v0.8.0 contained BusyBox (GPL-2.0) statically
linked against uClibc-ng (LGPL-2.1), taken from the Docker Official Image. Those
releases and images have been withdrawn. A private archive holds the
corresponding sources for each of those images. Keep it for at least three
years after the withdrawal date, which is recorded in the private release
records, and provide the sources on request.
