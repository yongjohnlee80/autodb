# internal/vocabguard

Static AST analysis utility that answers a critical correctness question: **Which constants of a given type does a Go package declare?**

---

## Why Source Analysis Over Reflection

In Go, untyped or typed constants leave no runtime metadata: a string constant cannot be interrogated at runtime to find which other constants share its type. 

Without `vocabguard`, test suites that verify that all enum variants are handled in a switch statement must manually maintain duplicate lists of constants. If an engineer adds a new constant to the production package but forgets to update the test slice, the test passes vacuously.

`vocabguard.Declared(dir, typeName)` parses the Go source files directly using `go/parser` and `go/ast`, returning the true set of declared constants in source order.

---

## Usage Example

```go
func TestAdmissionVerbs_Exhaustive(t *testing.T) {
    declared, files, err := vocabguard.Declared("../admission", "Verb")
    if err != nil {
        t.Fatalf("failed to parse admission package: %v", err)
    }
    if files == 0 {
        t.Fatal("parsed 0 files: invalid package path")
    }

    for _, name := range declared {
        // Assert that the verb is registered in the admission orchestrator
    }
}
```
