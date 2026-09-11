package exec

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const admissionEvidencePath = "../../docs/admission-pipeline-phase-1-evidence.md"

var admissionEvidenceIDs = []string{
	"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "A10", "A11", "A12",
	"A13", "A14", "A15", "A16", "A17", "A18", "A19", "A20", "A21", "A22", "A23", "A24",
	"A25", "A26", "A27",
}

func TestAdmissionEvidencePackage(t *testing.T) {
	doc := readAdmissionEvidence(t, admissionEvidencePath)
	rows := parseAdmissionEvidenceRows(t, doc)

	want := make(map[string]bool, len(admissionEvidenceIDs))
	for _, id := range admissionEvidenceIDs {
		want[id] = true
		if _, ok := rows[id]; !ok {
			t.Errorf("mandatory admission evidence row %s is missing", id)
		}
	}
	for id := range rows {
		if !want[id] {
			t.Errorf("unexpected evidence row %s; child rows are allowed only for the enumerated mandatory subclaims", id)
		}
	}

	placeholder := regexp.MustCompile(`(?i)(MISSING:|\b(?:TBD|TODO|PLACEHOLDER|UNOBSERVED|NOT[ -]RUN|N/A)\b)`)
	var incomplete []string
	for id, fields := range rows {
		for i, field := range fields {
			if strings.TrimSpace(field) == "" {
				t.Errorf("evidence row %s has an empty %s field", id, admissionEvidenceColumns()[i])
			}
			if placeholder.MatchString(field) {
				incomplete = append(incomplete, id)
				break
			}
		}
	}
	if len(incomplete) != 0 {
		sort.Strings(incomplete)
		t.Errorf("evidence rows contain MISSING/placeholders: %s", strings.Join(incomplete, ", "))
	}

	verifyAdmissionCorpusEvidence(t, doc)
	verifyAdmissionChainEvidence(t, doc)
}

func admissionEvidenceColumns() []string {
	return []string{
		"ID", "claim", "production mutation", "witness", "observed RED message",
		"restore proof", "restored green", "environment/source",
	}
}

func parseAdmissionEvidenceRows(t *testing.T, doc string) map[string][]string {
	t.Helper()
	rows := make(map[string][]string)
	inSection := false
	for _, line := range strings.Split(doc, "\n") {
		if line == "## Acceptance evidence" {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(line, "## ") {
			break
		}
		if !inSection || !strings.HasPrefix(line, "| `A") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 10 {
			t.Fatalf("evidence row has %d columns, want 8: %s", len(cells)-2, line)
		}
		fields := make([]string, 8)
		for i := range fields {
			fields[i] = strings.TrimSpace(cells[i+1])
		}
		id := strings.Trim(fields[0], "`")
		if _, exists := rows[id]; exists {
			t.Fatalf("duplicate admission evidence row %s", id)
		}
		fields[0] = id
		rows[id] = fields
	}
	if !inSection {
		t.Fatal("acceptance evidence section not found")
	}
	return rows
}

func verifyAdmissionCorpusEvidence(t *testing.T, doc string) {
	t.Helper()
	required := []string{
		"## Real corpus replay",
		"Status: OBSERVED PASS",
		"Corpus path: `/home/johno/Source/Projects/LabelManager/lm/ddex-delivery-replay/sql/deployments/scripts`",
		"Manifest SHA-256: `5b11c44fd97e0daef1d4981164de44dfa74667a5bf3ea852245baaa29a0172d3`",
		"Baseline diff: empty",
		"`TestCorpusReplay`: PASS",
		"Files: 470",
		"Empty files: 10",
		"Statements: 3577",
		"Decisions admitted: 2782",
		"Decisions control: 780",
		"Decisions nested: 1",
		"Decisions where: 14",
		"`TestParseTxControl_CorpusRoundTrip`: PASS",
		"Transaction controls: 756",
		"Transaction-control files: 470",
		"Transaction-control failures: 0",
	}
	for _, text := range required {
		if !strings.Contains(doc, text) {
			t.Errorf("real corpus evidence is missing %q", text)
		}
	}
}

func verifyAdmissionChainEvidence(t *testing.T, evidence string) {
	t.Helper()
	references := map[string][]string{
		"../../docs/admission-chain-comparison.md": {
			"admission-chain-baseline-aeac553:begin",
			"admission-chain-current:begin",
		},
		"admission_chain_render_test.go": {
			"TestAdmissionChainComparison_BaselineBlockIsFixedAtAeac553",
			"TestAdmissionChainComparison_CurrentBlockMatchesProduction",
			"TestAdmissionChainRendering_CriticalStageSlices",
		},
		"admission_ingress_test.go": {
			"TestAdmissionChainRendering_ProductionAndRendererShareFactories",
		},
	}
	for path, markers := range references {
		contents := readAdmissionEvidence(t, path)
		for _, marker := range markers {
			if !strings.Contains(contents, marker) {
				t.Errorf("machine-checked chain source %s is missing %q", path, marker)
			}
			if !strings.Contains(evidence, marker) {
				t.Errorf("evidence package does not reference chain check %q", marker)
			}
		}
	}
}

func readAdmissionEvidence(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
