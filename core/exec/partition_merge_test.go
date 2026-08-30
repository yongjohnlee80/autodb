package exec

import "testing"

// The two-snapshot merge (ADR-0077 fold 2): the base list is authoritative for
// which relations exist; the supplementary roles only annotate it. A base row
// is annotated when named, never dropped for lacking a match, and a
// supplementary-only relation (dropped between the two reads) is ignored. This
// is exactly the drift tolerance in both directions.
func TestMergePartitionRoles_AnnotateOnlyNeverDropIgnoreExtra(t *testing.T) {
	base := []TableEntry{
		{Name: "events"},         // annotated as a partitioned parent
		{Name: "events_2026_01"}, // annotated as a child
		{Name: "users"},          // no role — a detach between snapshots leaves it un-annotated
	}
	roles := map[string]partRole{
		"events":         {partitioned: true},
		"events_2026_01": {isPartition: true, parent: "events"},
		// A create+attach between the two reads: present in the later
		// (supplementary) snapshot but absent from the base — must be ignored,
		// not synthesized into a row.
		"events_2026_02": {isPartition: true, parent: "events"},
	}

	mergePartitionRoles(base, roles)

	if len(base) != 3 {
		t.Fatalf("base length = %d, want 3 — never drop a base row, never add a supplementary-only row", len(base))
	}
	if !base[0].Partitioned || base[0].IsPartition || base[0].Parent != "" {
		t.Errorf("events = %+v, want Partitioned only", base[0])
	}
	if base[1].Partitioned || !base[1].IsPartition || base[1].Parent != "events" {
		t.Errorf("events_2026_01 = %+v, want IsPartition parent=events", base[1])
	}
	if base[2].Name != "users" || base[2].Partitioned || base[2].IsPartition || base[2].Parent != "" {
		t.Errorf("users = %+v, want present and un-annotated", base[2])
	}
	for _, e := range base {
		if e.Name == "events_2026_02" {
			t.Fatal("a supplementary-only relation (create+attach mid-listing) was synthesized into the base list")
		}
	}
}
