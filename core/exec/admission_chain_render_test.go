package exec

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/admission"
)

const (
	baselineChainBegin = "<!-- admission-chain-baseline-aeac553:begin -->\n"
	baselineChainEnd   = "<!-- admission-chain-baseline-aeac553:end -->"
	currentChainBegin  = "<!-- admission-chain-current:begin -->\n```text\n"
	currentChainEnd    = "```\n<!-- admission-chain-current:end -->"
)

func TestAdmissionChainComparison_BaselineBlockIsFixedAtAeac553(t *testing.T) {
	doc, err := os.ReadFile("../../docs/admission-chain-comparison.md")
	if err != nil {
		t.Fatal(err)
	}
	block := markedChainBlock(t, string(doc), baselineChainBegin, baselineChainEnd)
	if got, want := fmt.Sprintf("%x", sha256.Sum256([]byte(block))), "529b7fced60a8ed360abf69c6e190edd84f6cebc44dcbe1554a3987ca4c091e9"; got != want {
		t.Fatalf("fixed aeac553 baseline block changed: sha256=%s, want %s", got, want)
	}
}

func TestAdmissionChainComparison_CurrentBlockMatchesProduction(t *testing.T) {
	doc, err := os.ReadFile("../../docs/admission-chain-comparison.md")
	if err != nil {
		t.Fatal(err)
	}
	committed := markedChainBlock(t, string(doc), currentChainBegin, currentChainEnd)
	if current := renderAdmissionChains(); committed != current {
		t.Fatalf("docs/admission-chain-comparison.md current block is not the production-derived rendering\n--- committed\n%s--- production\n%s", committed, current)
	}
}

func markedChainBlock(t *testing.T, text, begin, endMarker string) string {
	t.Helper()
	start := strings.Index(text, begin)
	if start < 0 {
		t.Fatalf("chain rendering begin marker %q not found", begin)
	}
	start += len(begin)
	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		t.Fatalf("chain rendering end marker %q not found", endMarker)
	}
	return text[start : start+end]
}

func TestAdmissionChainRendering_CriticalStageSlices(t *testing.T) {
	assertOrder := func(name string, got []string, want string) {
		t.Helper()
		if joined := strings.Join(got, " -> "); joined != want {
			t.Fatalf("%s stages moved or disappeared: got %q, want %q", name, joined, want)
		}
	}

	assertOrder("wire reported grammar", stageOrder(wireGrammarAdmissionStages(nil)), "reportedgrammar")
	assertOrder("extended Execute re-authorization", stageOrder(classAdmissionStages()), "authorizeunit")
	assertOrder("read-only enforcement capability", stageOrder(readOnlyEnforcementStages()), "readonlyenforcement")
	// The procedural stage sits with the profile: the extended protocol gates a
	// procedural verb through THIS chain, so the placement rule has to be in it.
	assertOrder("ordinary session policy", stageOrder(sessionAdmissionStages(ProfileV1Compat, nil)),
		"profile -> procedural -> readeranalysis -> authorizeunit -> guardwhere")
}

func stageOrder(stages []admission.Stage) []string {
	return admission.Compose(stages...).Order()
}
