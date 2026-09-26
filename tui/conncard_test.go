package tui

import (
	"strings"
	"testing"
)

func cardFixture() (ConnInfo, FrontDoorEndpoint) {
	return ConnInfo{Name: "bravo", TargetDB: "main", PoolMaxConns: 4}, FrontDoorEndpoint{
		Enabled: true, Listening: true, Addr: "127.0.0.1:5432",
		MaxSessionsPerUser: 8, MaxSessionsGlobal: 32, MaxTargetConns: 64,
	}
}

func TestCardBudget_TheCeilingsCarryTheirScope(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, want := range []string{"this connection        4", "sessions per user      8", "sessions, instance     32", "backend connections    64"} {
		if !strings.Contains(text, want) {
			t.Errorf("scope missing its own figure %q", want)
		}
	}
}

func TestCardBudget_AnUnreportedCeilingSaysSoRatherThanSayingZero(t *testing.T) {
	c, ep := cardFixture()
	ep.MaxSessionsPerUser, ep.MaxSessionsGlobal, ep.MaxTargetConns = 0, 0, 0
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "sessions per user") || strings.Contains(line, "sessions, instance") || strings.Contains(line, "backend connections") {
			if !strings.Contains(line, "not reported") {
				t.Errorf("an absent ceiling claimed a number: %q", line)
			}
		}
	}
}

func TestCardBudget_ItDoesNoArithmetic(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, recipe := range []string{"divide", "per process", "allotted share", "number of databases", "each client may"} {
		if strings.Contains(strings.ToLower(text), recipe) {
			t.Errorf("card invented a private share: %q", recipe)
		}
	}
}

func TestCardBudget_NoPerSourceConcurrencyCapIsAdvertised(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, claim := range []string{"per-source", "per source", "source concurrency"} {
		if strings.Contains(strings.ToLower(text), claim) {
			t.Errorf("failure throttle posed as capacity: %q", claim)
		}
	}
}

func TestCardBudget_ItTellsNobodyToConfigureAPool(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, claim := range []string{"setmaxopenconns", "maximumpoolsize", "pool size", "configure your pool"} {
		if strings.Contains(strings.ToLower(text), claim) {
			t.Errorf("card handed out pool sizing advice: %q", claim)
		}
	}
}

func TestCardBudget_ItShowsNoLiveAvailability(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	for _, claim := range []string{"free now", "available now", "currently free", "currently available"} {
		if strings.Contains(strings.ToLower(text), claim) {
			t.Errorf("show-once card claimed live availability: %q", claim)
		}
	}
}

func TestCardBudget_AnUnusableTokenGetsNoBudgetBlock(t *testing.T) {
	c, ep := cardFixture()
	ep.Listening = false
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	if !strings.HasPrefix(text, "!! THIS TOKEN CANNOT BE USED YET.") || strings.Contains(text, "LIMITS THAT APPLY") {
		t.Fatalf("unusable token was buried beneath budget advice")
	}
}

func TestCardBudget_TheConnectionsOwnBoundLeads(t *testing.T) {
	c, ep := cardFixture()
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	budget := strings.SplitN(text, "LIMITS THAT APPLY TO THIS TOKEN", 2)
	if len(budget) != 2 || !strings.Contains(budget[1], "this connection        4") ||
		strings.Index(budget[1], "this connection") > strings.Index(budget[1], "sessions per user") {
		t.Fatal("connection-owned bound did not lead the shared ceilings")
	}
}

func TestCardWarningAndCopyShareTheSameShownDetails(t *testing.T) {
	c, ep := cardFixture()
	ep.Cleartext = true
	text := buildCardText("fake-token", c, ep, "root", "admin", "")
	if !strings.HasPrefix(text, "!! THIS FRONT DOOR IS SERVING WITHOUT TLS") ||
		strings.Index(text, "CLEARTEXT") > strings.Index(text, "token        fake-token") ||
		!strings.Contains(text, "sslmode=disable") || !strings.Contains(text, "ssl=false") {
		t.Fatal("card failed to warn before the show-once credential or gave contradictory TLS advice")
	}
}

func TestCardURLsKeepSpecialCharactersInsideTheirFields(t *testing.T) {
	c, ep := cardFixture()
	c.TargetDB = "main/report? v"
	ep.RootCAFile = "/etc/my ca&key"
	text := buildCardText("fake%secret", c, ep, "a@b", "editor", "")
	for _, want := range []string{
		"database     main/report? v", // human field remains readable
		"a%40b:fake%25secret@127.0.0.1:5432/main%2Freport%3F%20v",
		"sslrootcert=%2Fetc%2Fmy+ca%26key",
		"user=a%40b", "password=fake%25secret",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("connection recipe lost an escaped field (missing expected fragment)")
		}
	}
}
