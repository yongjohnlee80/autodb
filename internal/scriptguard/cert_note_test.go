package scriptguard

// THE INSTALLER'S CERTIFICATE-FAILURE BRANCH, DRIVEN.
//
// autodb exits 78 (EX_CONFIG) when a failure is the CONFIGURATION rather than
// the command, and every subcommand loads the config — so `--create-cert` can
// fail for a reason that has nothing to do with certificates. On a 1 vCPU
// droplet it did, and this script said "--create-cert failed; leaving the front
// door disabled", which sent the operator to look at TLS.
//
// I shipped that branch with its syntax checked and its output UNWITNESSED, and
// said so — review asked for it closed rather than named, which is the right
// call: a branch whose text nobody has watched print is a branch whose text
// might not print.
//
// Contained: no host, no docker, no provisioning. install_frontdoor.sh has a
// define-only mode that stops with every function defined and nothing done, so
// a cell can source it and drive one function.

import (
	"os/exec"
	"strings"
	"testing"
)

// certNote runs cert_failure_note with the given exit code, through the real
// script.
func certNote(t *testing.T, rc string) string {
	t.Helper()
	script := installer(t)
	cmd := exec.Command("sh", "-c",
		"AUTODB_INSTALL_DEFINE_ONLY=1 . "+script+" >/dev/null 2>&1; cert_failure_note "+rc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driving cert_failure_note %s: %v\n%s", rc, err, out)
	}
	return string(out)
}

func TestCertNote_AConfigFailureIsNotReportedAsACertificateFailure(t *testing.T) {
	t.Parallel()

	got := certNote(t, "78")
	if !strings.Contains(got, "the CONFIG is invalid") {
		t.Errorf("exit 78 is not reported as a configuration problem: %s", got)
	}
	if !strings.Contains(got, "certificate generation never ran") {
		t.Errorf("the note does not say certificate generation never happened, which is "+
			"the fact that stops an operator debugging TLS: %s", got)
	}
	// AND IT DOES NOT SAY THE OTHER THING. Both branches printing would leave
	// the operator with the same two candidate causes they had before.
	if strings.Contains(got, "--create-cert failed") {
		t.Errorf("exit 78 also prints the generic certificate failure, so the framing is "+
			"no better than before: %s", got)
	}
}

// AND THE POSITIVE CONTROL: an ORDINARY failure keeps the certificate wording.
//
// Without it, a branch that reported "the CONFIG is invalid" for every failure
// would satisfy the cell above — and would misdirect every real certificate
// problem, which is the same defect pointed the other way.
func TestCertNote_AnOrdinaryFailureKeepsTheCertificateWording(t *testing.T) {
	t.Parallel()

	for _, rc := range []string{"1", "2", "127"} {
		got := certNote(t, rc)
		if !strings.Contains(got, "--create-cert failed") {
			t.Errorf("exit %s does not report a certificate failure: %s", rc, got)
		}
		if strings.Contains(got, "the CONFIG is invalid") {
			t.Errorf("exit %s is reported as a configuration problem: %s", rc, got)
		}
	}
}

// THE DEFINE-ONLY MODE DOES NOTHING, which is what makes the two cells above
// safe to run anywhere.
//
// A seam that quietly detected the host, wrote a file or started something
// would turn every future cell built on it into a side effect.
func TestCertNote_DefineOnlyModeHasNoSideEffects(t *testing.T) {
	t.Parallel()

	script := installer(t)
	cmd := exec.Command("sh", "-c", "AUTODB_INSTALL_DEFINE_ONLY=1 . "+script+"; echo SOURCED")
	out, err := cmd.CombinedOutput()
	body := string(out)
	if err != nil {
		t.Fatalf("sourcing in define-only mode failed: %v\n%s", err, body)
	}
	if !strings.Contains(body, "SOURCED") {
		t.Fatalf("the script did not return control to the caller: %s", body)
	}
	// The phases it would otherwise announce. Their absence is the claim.
	for _, phase := range []string{"cpu / ram", "Sizing", "preflight", "=== "} {
		if strings.Contains(body, phase) {
			t.Errorf("define-only mode reached %q, so it is not side-effect free: %s",
				phase, body)
		}
	}
}
