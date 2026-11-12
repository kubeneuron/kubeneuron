# Release checklist

Work through this list, in order, before pushing a `vX.Y.Z` release tag.
Release tags (`refs/tags/v*`) can be created only by repository admins.

## 1. Gates

- [ ] `make gates` passes on the release commit.
- [ ] The hardware GPU E2E run is green for the release commit (dispatch it,
      or confirm the latest weekly run is green). See
      [Hardware E2E dispatch](hw-e2e-dispatch.md).
- [ ] CI is green on `main` at the release commit, including the `CLA` check.

## 2. Adapter contract modules (required before the next release)

`pkg/accelerator` and `pkg/cloud` are separate Go modules (the Apache-2.0
adapter contract; see [LICENSE](https://github.com/kubeneuron/kubeneuron/blob/main/LICENSE),
section 5). Until they are tagged, the root `go.mod` requires them at a
placeholder version (`v0.0.0-00010101000000-000000000000`) through local
`replace` directives. A `replace` is ignored outside this repository, so
`go install github.com/kubeneuron/kubeneuron/cmd/...@vX.Y.Z` and any module
that depends on the root module would fail to resolve the placeholder.

Before the first release that contains these modules:

- [ ] Tag the modules on the release commit (or an earlier commit with the
      same module contents), then push the tags:

      ```sh
      git tag pkg/accelerator/v0.1.0
      git tag pkg/cloud/v0.1.0
      git push origin pkg/accelerator/v0.1.0 pkg/cloud/v0.1.0
      ```

- [ ] Require the real versions in the root module and commit the result:

      ```sh
      go mod edit \
        -require=github.com/kubeneuron/kubeneuron/pkg/accelerator@v0.1.0 \
        -require=github.com/kubeneuron/kubeneuron/pkg/cloud@v0.1.0
      go mod tidy
      ```

      The `replace` directives may stay: they keep in-repository builds on
      the local source, while external builds resolve the tagged versions.

- [ ] Confirm the modules resolve from the module proxy:

      ```sh
      GOFLAGS=-mod=mod go list -m github.com/kubeneuron/kubeneuron/pkg/accelerator@v0.1.0
      GOFLAGS=-mod=mod go list -m github.com/kubeneuron/kubeneuron/pkg/cloud@v0.1.0
      ```

On later releases, tag a new `pkg/accelerator/vX.Y.Z` or `pkg/cloud/vX.Y.Z`
only when that module changed, and require it in the root `go.mod` before
tagging the release.

## 3. Tag and verify

- [ ] Push the `vX.Y.Z` tag; the release workflow builds, signs, and
      publishes a draft, verifies it, and publishes it.
- [ ] Run `bash hack/verify-release.sh vX.Y.Z kubeneuron/kubeneuron` against
      the published release.
