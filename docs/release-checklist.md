# Release checklist

Work through this list, in order, before pushing a `vX.Y.Z` release tag.
Release tags (`refs/tags/v*`) and adapter contract module tags
(`refs/tags/pkg/accelerator/v*`, `refs/tags/pkg/cloud/v*`) can be created,
moved, or deleted only by repository admins (repository ruleset "release
tags: admins only").

## 1. Gates

- [ ] `make gates` passes on the release commit.
- [ ] The hardware GPU E2E run is green for the release commit (dispatch it,
      or confirm the latest weekly run is green). See
      [Hardware E2E dispatch](hw-e2e-dispatch.md).
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

- [ ] Push the `vX.Y.Z` tag; the release workflow builds, signs, and
      publishes a draft, verifies it, and publishes it.
- [ ] Run `bash hack/verify-release.sh vX.Y.Z kubeneuron/kubeneuron` against
      the published release.

## Native component source retention

Before publishing an image, run `python3 -I hack/bundle-native-sources.py
--output dist/native-sources-busybox-1.38.0.tar.gz` (on one line). The source
lock in `build/licenses/busybox/sources.lock.json` must match the Dockerfile
image digest. It records the BusyBox source, linked uClibc source, Buildroot
and the per-architecture upstream build recipes and patches, with SHA-256
checksums. Changing the image requires reviewing and updating that lock.

The release workflow publishes this archive as a permanent release asset,
covered by the release checksum/signature. The one-day Actions artifact is
only a transport copy. Confirm the published archive can be downloaded and
retain an owner-controlled backup before distributing images. Keep both
copies for at least three years after the last distribution of any image
containing this BusyBox binary; do not delete them when retiring a release.
Record the last distribution date in the private release records. Review the
complete corresponding-source contents, including linked-library obligations,
when changing the build. An upstream download link alone is not fulfillment
of the written offer in the image.

The agent must not contain NVIDIA's packaged `dcgmi`, its DCGM libraries, or
its added `libgcc_s` dependency. Package license review did not permit their
redistribution; retaining license files does not grant that permission.
`make verify-image` checks that DCGM binaries and their copied legal files
are absent. Users must supply their own compatible client explicitly through
`hostTooling.dcgmiPath` when DCGM runtime attestation is required.
