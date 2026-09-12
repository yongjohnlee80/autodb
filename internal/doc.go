// Package internal provides private testing, static analysis, and verification
// utilities for autodb.
//
// Subpackages within internal are strictly non-production support tools:
//
//   - internal/commentguard:
//     AST ratchet that enforces public accessibility and documentation standards
//     on in-code comments, preventing leakage of unlinked private references.
//
//   - internal/vocabguard:
//     AST analysis engine that parses Go package declarations to verify the
//     exhaustiveness of constant type vocabularies without relying on runtime reflection.
//
//   - internal/scriptguard:
//     Integration test harness that exercises host provisioning, installation,
//     and uninstallation shell scripts (install.sh, install_frontdoor.sh, etc.)
//     against isolated sandbox environments.
package internal
