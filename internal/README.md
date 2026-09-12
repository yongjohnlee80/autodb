# internal

Private test harnesses, static analysis guards, and infrastructure verification tools for `autodb`. Packages under `internal/` contain zero production code and cannot be imported by external Go modules.

---

## Subpackages

| Subpackage | Role & Purpose |
| :--- | :--- |
| **`commentguard`** | Enforces comment accessibility standards and prevents private document citations from leaking into production Go source code. |
| **`vocabguard`** | Performs static AST analysis over constant declarations to guarantee exhaustiveness in finite type vocabularies. |
| **`scriptguard`** | Validates installation, update, uninstallation, and provisioning shell scripts against isolated environments. |

---

## Verification Architecture

```
+─────────────────────────────────────────────────────────────────────────+
|                           autodb Verification                           |
+────────────────────────────────────┬────────────────────────────────────+
                                     │
         ┌───────────────────────────┼───────────────────────────┐
         ▼                           ▼                           ▼
+─────────────────+         +─────────────────+         +─────────────────+
|  commentguard   |         |   vocabguard    |         |   scriptguard   |
|  * AST ratchet  |         |  * AST parser   |         |  * Shell test   |
|  * Certified pkg|         |  * Exhaustive   |         |  * Install/Unit |
|    integrity    |         |    constants    |         |  * Rollback     |
+─────────────────+         +─────────────────+         +─────────────────+
```
