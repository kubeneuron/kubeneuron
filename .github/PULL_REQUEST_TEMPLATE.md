**What does this change?**

**Why?**

**Checklist**
- [ ] `make build && go test -race ./...` passes locally
- [ ] `make verify-generate` is clean (if `api/` changed)
- [ ] Docs updated (README/docs/) if behavior or install steps changed
- [ ] No new path can execute a destructive action without the documented
      gates (see `docs/design.md` and `PRODUCT_PLAN.md` safety rules)
- [ ] `CHANGELOG.md` updated under `[Unreleased]`
