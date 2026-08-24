package webserver

// The manifest for notes_mode_test.go, kept in a SEPARATE FILE on purpose.
//
// The r2 commit lost five verified tests to an edit that replaced from a marker
// to end-of-file. The first version of this guard lived at the end of that same
// file, so repeating the exact edit would have deleted the guard along with the
// tests it guards and the suite would have gone green again (lector r4).
//
// Here, that edit instead leaves this file referencing symbols that no longer
// exist — a COMPILE failure, which cannot be mistaken for a pass. A guard that
// can be removed by the failure it guards against is not a guard.

import "testing"

func TestNotesMode_TestInventory(t *testing.T) {
	// Naming each test binds it: deleting one breaks the build here.
	required := map[string]func(*testing.T){
		"WorkspaceRequiresBoundSubject":                  TestNotesMode_WorkspaceRequiresBoundSubject,
		"BoundSubjectReadsTheSharedTree":                 TestNotesMode_BoundSubjectReadsTheSharedTree,
		"WorkspaceRefusesAnotherSubject":                 TestNotesMode_WorkspaceRefusesAnotherSubject,
		"WrongSubjectCannotBootstrap":                    TestNotesMode_WrongSubjectCannotBootstrap,
		"DefaultIsPerUserIsolation":                      TestNotesMode_DefaultIsPerUserIsolation,
		"UnsafeSubjectStillRefused":                      TestNotesMode_UnsafeSubjectStillRefused,
		"UnsafeConfiguredSubjectIsRefusedAtConstruction": TestNotesMode_UnsafeConfiguredSubjectIsRefusedAtConstruction,
		"UnsafeSubjectCannotBootstrap":                   TestNotesMode_UnsafeSubjectCannotBootstrap,
		"EffectiveRootDiffersByMode":                     TestNotesMode_EffectiveRootDiffersByMode,
		"AboutReportsTheEffectiveRoot":                   TestNotesMode_AboutReportsTheEffectiveRoot,
		"RunnerBuildsTheModelWithTheEffectiveRoot":       TestNotesMode_RunnerBuildsTheModelWithTheEffectiveRoot,
	}
	const want = 11
	if len(required) != want {
		t.Errorf("manifest lists %d tests, want %d: a test was added or removed without "+
			"updating this manifest", len(required), want)
	}
	for name, fn := range required {
		if fn == nil {
			t.Errorf("required test %q is nil", name)
		}
	}
}
