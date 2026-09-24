package tui

import (
	"errors"
	"strings"
	"testing"
)

// THE RESTART PROMPT.
//
// The decided semantics are cancel-and-wait: a restart ABORTS a running
// statement rather than letting it finish. The confirmation has to state that
// and name how many are in flight.
//
// There was no confirmation at all. restartServer ran its refusal checks --
// browser frontend, service host or client_only, not connected -- and then went
// straight to setStatus("restarting the server…"). So an operator restarted the
// front door with no statement of consequence and no figure, and whatever was
// mid-statement was cancelled without warning.

func TestRestartQuestion_StatesTheConsequenceAndNamesTheFigure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		f       InFlight
		readErr error
		want    []string
		absent  []string
	}{
		{
			name: "one statement is named as one",
			f:    InFlight{Executing: 1},
			want: []string{"1 statement", "WILL BE CANCELLED"},
		},
		{
			name: "several are counted",
			f:    InFlight{Executing: 4},
			want: []string{"4 statements", "WILL BE CANCELLED"},
		},
		{
			// AND ZERO IS NOT SILENCE. "Nothing is running" invites the
			// assumption that nothing can be lost, when a statement started
			// between the reading and the answer is cancelled like any other.
			name: "none still says what a restart does",
			f:    InFlight{Executing: 0},
			want: []string{"No statement was running", "WILL BE CANCELLED"},
		},
		{
			// A FAILED READING IS NOT A REASON TO GO QUIET, and it is not a
			// reason to refuse either: an unrelated failure must not block an
			// operator's recovery action.
			name:    "an unreadable figure still warns",
			f:       InFlight{},
			readErr: errors.New("connection reset"),
			want:    []string{"WILL BE CANCELLED", "could not be read", "connection reset"},
		},
		{
			// THE OTHER POPULATION, NAMED AS ITSELF. Open transactions are not
			// statements in flight, and a restart does not silently destroy
			// them -- the server refuses while any are open. Calling them work
			// about to be cancelled would be untrue in the alarming direction.
			name: "an open transaction is described as refusal, not loss",
			f:    InFlight{Executing: 2, InTransaction: 3},
			want: []string{"2 statements", "3 sessions hold an open transaction", "REFUSES"},
		},
		{
			name:   "no transactions, no sentence about them",
			f:      InFlight{Executing: 2},
			absent: []string{"transaction"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := restartQuestion(tc.f, tc.readErr)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the prompt does not carry %q:\n%s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("the prompt mentions %q when it does not apply:\n%s", a, got)
				}
			}
			// EVERY variant says the figures are a reading. Between drawing the
			// prompt and the operator answering, sessions open and close; a
			// prompt implying the number still holds at confirm time would be
			// making a promise the server does not keep.
			if !strings.Contains(got, "when this prompt was drawn") {
				t.Errorf("the prompt presents its figures as though still true at confirm "+
					"time:\n%s", got)
			}
		})
	}
}
