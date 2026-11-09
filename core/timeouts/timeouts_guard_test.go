package timeouts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// guarded are the packages whose timeouts must come from this one, relative
// to this package's directory.
var guarded = []string{"../exec", "../config", "../../frontdoor"}

// notATimeout is the annotation that lets a time.<unit> stay where it is: a
// ticker, an interval, a cache lifetime, a unit conversion.
const notATimeout = "// not a timeout: "

var units = map[string]bool{
	"Nanosecond": true, "Microsecond": true, "Millisecond": true,
	"Second": true, "Minute": true, "Hour": true,
}

// strays returns every time.<unit> in f whose line carries no notATimeout
// annotation, as "file:line", and how many annotated ones it passed.
func strays(fset *token.FileSet, f *ast.File) (found []string, annotated int) {
	timeName := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "time" {
			timeName = "time"
			if imp.Name != nil {
				timeName = imp.Name.Name
			}
		}
	}
	if timeName == "" {
		return nil, 0
	}
	excused := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, notATimeout) && len(strings.TrimSpace(c.Text[len(notATimeout):])) > 0 {
				excused[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !units[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != timeName {
			return true
		}
		pos := fset.Position(sel.Pos())
		if excused[pos.Line] {
			annotated++
			return true
		}
		found = append(found, pos.Filename+":"+strconv.Itoa(pos.Line))
		return true
	})
	return found, annotated
}

// Every timeout in core/exec, core/config and frontdoor comes from this
// package. A time.<unit> anywhere else in them must say why it is not one.
func TestGuard_EveryTimeoutLivesInOnePlace(t *testing.T) {
	fset := token.NewFileSet()
	var files, annotated int
	for _, dir := range guarded {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", p, err)
			}
			files++
			found, n := strays(fset, f)
			annotated += n
			for _, s := range found {
				t.Errorf("%s: a time.<unit> outside core/timeouts; move the timeout there, or annotate the line %q<kind>", s, notATimeout)
			}
		}
	}
	// A guard that read nothing passes everything: prove it read the packages
	// and passed the annotated sites they hold.
	if files < 50 || annotated == 0 {
		t.Fatalf("the guard read %d files and passed %d annotated sites; it is not reading the guarded packages", files, annotated)
	}
}

// The guard's own check, on sources it must refuse and pass.
func TestGuard_FindsAStrayAndPassesAnAnnotation(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"a re-added timeout", "package p\nimport \"time\"\nconst queueWait = 90 * time.Second\n", 1},
		{"a bare unit", "package p\nimport \"time\"\nvar d = time.Minute\n", 1},
		{"an aliased import", "package p\nimport tm \"time\"\nvar d = 2 * tm.Hour\n", 1},
		{"an annotated ticker", "package p\nimport \"time\"\nvar d = time.Second // not a timeout: a ticker\n", 0},
		{"an annotation with no kind", "package p\nimport \"time\"\nvar d = time.Second // not a timeout: \n", 1},
		{"the annotation on another line", "package p\nimport \"time\"\n// not a timeout: a ticker\nvar d = time.Second\n", 1},
		{"a time.Duration conversion", "package p\nimport \"time\"\nvar d = time.Duration(5)\n", 0},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", c.src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, _ := strays(fset, f); len(got) != c.want {
			t.Errorf("%s: %d strays %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

// Every fixed timeout is held to MaxBound too, though no reload can raise it.
func TestFixedTimeoutsAreWithinMaxBound(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"TLSHandshake": TLSHandshake, "StartupPacket": StartupPacket, "CredentialExchange": CredentialExchange,
		"QueueWait": QueueWait, "HandshakeReserve": HandshakeReserve, "PinAllowance": PinAllowance, "AdmitFinalize": AdmitFinalize,
		"MemoryLaneWait": MemoryLaneWait, "ReclaimGrace": ReclaimGrace, "IdleBetweenMessages": IdleBetweenMessages,
		"ServerBeltMargin": ServerBeltMargin, "IdleHoldingState": IdleHoldingState,
		"StrayBackendIdle": StrayBackendIdle, "BackendReset": BackendReset, "BackendConnect": BackendConnect,
		"CancelDialFallback": CancelDialFallback, "OutputStall": OutputStall, "FrameStall": FrameStall,
		"SegmentStall": SegmentStall, "GoodbyeWrite": GoodbyeWrite, "Record": Record, "AuditWrite": AuditWrite,
		"TxCleanup": TxCleanup, "EngineClose": EngineClose, "CloseQuiesce": CloseQuiesce,
		"PoolClose": PoolClose, "Quiesce": Quiesce, "RemoteHandshake": RemoteHandshake,
		"RemoteReconnectGrace": RemoteReconnectGrace,
		// The policy and config defaults start within it as well.
		"SessionIdle": SessionIdle, "IdleInTransaction": IdleInTransaction, "MaxTransaction": MaxTransaction,
		"MaxTransactionCeiling": MaxTransactionCeiling, "BackendIdle": BackendIdle, "BackendLifetime": BackendLifetime,
	} {
		if d <= 0 || d > MaxBound {
			t.Errorf("%s is %v; every timeout is positive and at most MaxBound (%v)", name, d, MaxBound)
		}
	}
}

// The test reads the guarded packages from disk; this keeps a moved package
// from turning the guard into a no-op silently.
func TestGuard_TheGuardedPackagesExist(t *testing.T) {
	for _, dir := range guarded {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("guarded package %s: %v", dir, err)
		}
	}
}

// A waiting front-door login has QueueWait, after its credential phase. Serving
// it by reclamation takes the victim's grace and then its goodbye frame, whose
// write is bounded by GoodbyeWrite, before the lease comes back. If those two
// reach the wait, a login that arrived just as every holder went quiet is refused
// before any lease can be freed for it. And the pin runs inside the finalization
// budget, so it can never be the larger of the two.
func TestReclaimGrace_FitsInsideAFrontDoorLoginsWait(t *testing.T) {
	if path := ReclaimGrace + GoodbyeWrite; path >= QueueWait {
		t.Errorf("ReclaimGrace (%v) + GoodbyeWrite (%v) = %v is not shorter than a front-door "+
			"login's wait (%v)", ReclaimGrace, GoodbyeWrite, path, QueueWait)
	}
	if PinAllowance >= AdmitFinalize {
		t.Errorf("PinAllowance (%v) is not inside AdmitFinalize (%v)", PinAllowance, AdmitFinalize)
	}
}
