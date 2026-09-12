# internal/scriptguard

Integration test suite and regression harnesses for `autodb`'s operational shell scripts:
- `install.sh`
- `install_frontdoor.sh`
- `update_frontdoor.sh`
- `uninstall.sh`
- `provision_vm.sh`

---

## The Need for Executable Script Tests

Shell scripts often fail on subtle edge cases that `sh -n` syntax checks cannot detect:
1. **Undefined Function Calls**: A helper invoked after early validation passes.
2. **`set -e` Traps**: A cleanup helper (such as `rm_file`) ending in `[ -e "$f" ]` that evaluates to non-zero when the target file is already absent, causing immediate script termination midway through an uninstallation.
3. **Idempotence & Rollbacks**: Ensuring failed installations or updates roll back cleanly to previous states without stranding half-configured systemd service units.

---

## Testing Model

Tests execute scripts against disposable Docker containers or mocked root directories:
- Validates `--dry-run` vs `--apply` modes.
- Asserts exit code semantics (`0` on clean success, `EX_CONFIG` or non-zero on validation failures).
- Checks non-TTY refusals when required interactive flags are omitted.
