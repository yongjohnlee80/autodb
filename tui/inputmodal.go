package tui

// A FACTORY FOR THE MODALS, over the machinery that already exists.
//
// Every modal in this package was hand-assembled: a []formField here, an
// openDialog with three answers there, each one repeating the same decisions
// about buttons, submission and what a failed validation does. The shapes had
// already drifted — some validated before submitting, some after; some kept
// the modal open on a bad value, some closed it.
//
// THIS IS A FAÇADE, NOT A SECOND MODAL STACK. It builds the same formField
// rows and hands them to the same openFormOpts, so the overlay registry, the
// scrim rules and the dismissal semantics keep applying without being restated
// — a parallel stack would have to reimplement all three, and reimplementing
// them is how they drifted apart the first time.
//
// The confirm modal is not a separate type for the same reason: a confirmation
// is an input modal with no inputs and one button, so it is spelled that way
// rather than given a parallel implementation that can disagree.

import (
	"errors"
	"fmt"
	"sort"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// ModalStatus is how a modal ended.
type ModalStatus uint8

const (
	// StatusSubmitted: the operator confirmed, and every validator passed.
	StatusSubmitted ModalStatus = iota
	// StatusCancelled: the operator declined, by the cancel button, Escape or
	// a host dismiss key. Bindings are NOT written.
	StatusCancelled
)

func (s ModalStatus) String() string {
	switch s {
	case StatusSubmitted:
		return "submitted"
	case StatusCancelled:
		return "cancelled"
	}
	return "unknown"
}

// ModalResponse is what a submit function receives.
//
// Values is carried as well as the bindings because a submit function often
// needs a value it did not bind — an id it is about to pass to an RPC, say —
// and reaching back into the widget after the modal has closed is how a
// use-after-close bug gets written.
type ModalResponse struct {
	Status ModalStatus
	Values formValues
}

// ErrEmptyText is the validation every text input gets for free when it is
// marked required. Exported because submit functions compare against it.
var ErrEmptyText = errors.New("this field cannot be empty")

// Input is one row of a modal: a text field, a select, or a line of prose.
//
// It is an interface rather than a struct with a kind tag because a select's
// option type varies by row and a heterogeneous slice cannot carry a type
// parameter — the same reason formField.build is a closure.
type Input interface {
	// field is the form row this input contributes, and ok is FALSE for a row
	// that is not a field at all — prose.
	//
	// Prose is excluded structurally rather than by a "focusable" flag on the
	// contrary: a non-field row never reaches f.fields, so it cannot take the
	// cursor, cannot occupy a formValues position, and cannot push every
	// later row's binding index off by one. A flag would have left all three
	// as things each caller had to get right.
	field() (formField, bool)
	// chrome is the component a non-field row contributes. nil for a field.
	//
	// A COMPONENT RATHER THAN A STRING, so the row can be a divider as easily
	// as a line of prose — and so a caller can place either BETWEEN two inputs
	// rather than only above them all.
	chrome() tui.Component
	// commit writes the submitted value through the caller's binding. It runs
	// ONLY on StatusSubmitted and only after every validator has passed, so a
	// cancelled or invalid modal cannot leave a half-written struct behind.
	//
	// i is the row's position among the FIELDS, not among the arguments to
	// NewInputModal, because that is what indexes formValues.
	commit(v formValues, i int)
	// validate reports why this row is unacceptable, or nil.
	validate(v formValues, i int) error
}

// --- text -------------------------------------------------------------------

type textInputSpec struct {
	label     string
	bind      *string
	old       string
	masked    bool
	required  bool
	validate_ func(string) error
}

// NewTextInput is a labelled free-text row bound to a string.
//
// The binding is a pointer so a submit function reads ordinary variables
// rather than indexing a values slice by a position it has to keep in step
// with the argument order.
func NewTextInput(label string, bind *string) *textInputSpec {
	return &textInputSpec{label: label, bind: bind}
}

// WithOldValue prefills the row with the current value, which is what makes a
// rename a rename rather than a retype.
func (t *textInputSpec) WithOldValue(v string) *textInputSpec { t.old = v; return t }

// Masked hides the text. For passphrases.
func (t *textInputSpec) Masked() *textInputSpec { t.masked = true; return t }

// Required refuses an empty value with ErrEmptyText.
func (t *textInputSpec) Required() *textInputSpec { t.required = true; return t }

// WithValidateFn adds a caller's check. It runs after Required, so a validator
// never has to re-check for empty.
func (t *textInputSpec) WithValidateFn(fn func(string) error) *textInputSpec {
	t.validate_ = fn
	return t
}

func (t *textInputSpec) field() (formField, bool) {
	opts := []widget.TextInputOption{}
	if t.masked {
		opts = append(opts, widget.WithMask('*'))
	}
	if t.old != "" {
		opts = append(opts, widget.WithInitialValue(t.old))
	}
	return field(t.label, opts...), true
}

func (t *textInputSpec) chrome() tui.Component { return nil }

func (t *textInputSpec) validate(v formValues, i int) error {
	s := v.str(i)
	if t.required && s == "" {
		return ErrEmptyText
	}
	if t.validate_ != nil {
		return t.validate_(s)
	}
	return nil
}

func (t *textInputSpec) commit(v formValues, i int) {
	if t.bind == nil {
		return
	}
	// RAW, NOT TRIMMED, when masked: a passphrase's spaces are the operator's.
	if t.masked {
		*t.bind = v.raw(i)
		return
	}
	*t.bind = v.str(i)
}

// --- select -----------------------------------------------------------------

// ErrNoChoice is what a required select refuses with.
var ErrNoChoice = errors.New("choose one")

type selectInputSpec struct {
	label     string
	bind      *int64
	options   map[int64]string
	required  bool
	validate_ func(int64) error
}

// NewSelectInput is a row that chooses one row identity from a catalogue
// already in hand — the connections not yet in a workspace, say.
//
// The options are a map because the caller has one: an id keyed to what the
// operator should read. THE ORDER IS IMPOSED HERE, by label, because Go
// randomises map iteration and a list that reshuffles between two openings of
// the same modal is one an operator cannot learn. The id breaks ties so two
// rows sharing a label still come out in a stable order rather than swapping.
func NewSelectInput(label string, options map[int64]string, bind *int64) *selectInputSpec {
	return &selectInputSpec{label: label, bind: bind, options: options}
}

// Required refuses "no choice" with ErrNoChoice.
func (t *selectInputSpec) Required() *selectInputSpec { t.required = true; return t }

// WithValidateFn adds a caller's check on the chosen id. It runs after
// Required, so a validator never has to re-check for absence.
func (t *selectInputSpec) WithValidateFn(fn func(int64) error) *selectInputSpec {
	t.validate_ = fn
	return t
}

// items is the catalogue in the order the operator will see it. Separate from
// field because the order is the whole of what there is to get wrong here, and
// this is the only place that decides it.
func (t *selectInputSpec) items() []widget.SelectItem[int64] {
	ids := make([]int64, 0, len(t.options))
	for id := range t.options {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if t.options[ids[a]] != t.options[ids[b]] {
			return t.options[ids[a]] < t.options[ids[b]]
		}
		return ids[a] < ids[b]
	})
	items := make([]widget.SelectItem[int64], 0, len(ids))
	for _, id := range ids {
		items = append(items, widget.SelectItem[int64]{Value: id, Label: t.options[id]})
	}
	return items
}

func (t *selectInputSpec) field() (formField, bool) {
	return fixedSelect(t.label, t.items()), true
}

func (t *selectInputSpec) chrome() tui.Component { return nil }

func (t *selectInputSpec) validate(v formValues, i int) error {
	id, ok := v.id(i)
	if t.required && !ok {
		return ErrNoChoice
	}
	if t.validate_ != nil {
		return t.validate_(id)
	}
	return nil
}

func (t *selectInputSpec) commit(v formValues, i int) {
	if t.bind == nil {
		return
	}
	// AN ABSENT CHOICE WRITES ZERO, and that is deliberate rather than
	// skipped: a caller that did not mark the row Required has said an empty
	// choice is acceptable, and leaving the binding at whatever it held before
	// would report the previous modal's answer as this one's.
	id, _ := v.id(i)
	*t.bind = id
}

// --- prose ------------------------------------------------------------------

type textValueSpec struct{ text_ string }

// NewTextValue is a line of prose inside the modal — the question a
// confirmation asks, or a warning above the inputs. It takes no cursor.
func NewTextValue(text string) *textValueSpec { return &textValueSpec{text_: text} }

func (t *textValueSpec) field() (formField, bool) { return formField{}, false }
func (t *textValueSpec) chrome() tui.Component {
	return widget.NewText(t.text_, widget.WithWrapMode(widget.Wrap))
}
func (t *textValueSpec) validate(formValues, int) error { return nil }
func (t *textValueSpec) commit(formValues, int)         {}

// --- a divider ----------------------------------------------------------------

type hruleSpec struct{}

// NewHorizontalRule is a divider row, placed like any other input.
//
// NOT AUTOMATIC BETWEEN FIELDS. A login form of two rows wants one; a form of
// six related fields would be cut into six pieces by the same rule applied by
// default, and a divider that appears everywhere separates nothing. The caller
// says where the groups are, because the caller is the one who knows.
func NewHorizontalRule() *hruleSpec { return &hruleSpec{} }

func (h *hruleSpec) field() (formField, bool)       { return formField{}, false }
func (h *hruleSpec) chrome() tui.Component          { return newHRule() }
func (h *hruleSpec) validate(formValues, int) error { return nil }
func (h *hruleSpec) commit(formValues, int)         {}

// --- the modal --------------------------------------------------------------

// InputModal is the builder. Nothing happens until Open.
type InputModal struct {
	m          *Model
	title      string
	inputs     []Input
	okText     string
	cancelText string
	okMnemonic rune
	noCancel   bool
	scrim      bool
	submit     func(ModalResponse) error
	cancel     func(ModalResponse)
}

// NewInputModal starts a modal with the given rows.
func NewInputModal(m *Model, title string, inputs ...Input) *InputModal {
	return &InputModal{m: m, title: title, inputs: inputs, okText: "OK"}
}

// WithSubmitFn runs on confirmation, AFTER every validator has passed and
// every binding has been written.
//
// Returning an error keeps the modal open and shows the message, which is what
// makes a server-side refusal — a name already taken — behave like a
// validation failure instead of closing the modal on a change that did not
// happen.
func (b *InputModal) WithSubmitFn(fn func(ModalResponse) error) *InputModal {
	b.submit = fn
	return b
}

// WithCancelFn runs when the operator declines — by the Cancel button, by
// Escape, or by any other route that dismisses the surface, INCLUDING on a
// modal built with ExcludeCancel, which has no cancel button for Escape to
// resolve against.
//
// It receives a ModalResponse like the submit function does, so one handler
// can serve both and read Status rather than being told which it is by which
// function ran. Values is the answers as they stood, and NO BINDING HAS BEEN
// WRITTEN — a cancelled modal must not change the caller's data.
func (b *InputModal) WithCancelFn(fn func(ModalResponse)) *InputModal { b.cancel = fn; return b }

// WithOkText renames the confirming button. "CONFIRM", "Delete", "Sign in".
func (b *InputModal) WithOkText(s string) *InputModal { b.okText = s; return b }

// WithCancelText renames the declining button.
//
// Worth using whenever the affirmative names an ACT rather than an agreement:
// "Quit" beside "Stay" names the two outcomes, where "Quit" beside "Cancel"
// names one outcome and a refusal to choose. The declining button's mnemonic
// follows its new text.
func (b *InputModal) WithCancelText(s string) *InputModal { b.cancelText = s; return b }

// WithOkMnemonic gives the affirmative a bare-letter accelerator.
//
// ONLY FOR MODALS WITH NO INPUTS, and it panics otherwise. In a form every
// bare letter is a character somebody may type, which is how the affirmative's
// old 'O' came to fire mid-word; a confirmation has nothing to type into.
func (b *InputModal) WithOkMnemonic(r rune) *InputModal { b.okMnemonic = r; return b }

// ExcludeCancel leaves only the confirming button.
//
// FOR ACKNOWLEDGEMENTS, NOT FOR DECISIONS. A modal that asks a question must
// keep a way to say no; this is for the ones that only report.
func (b *InputModal) ExcludeCancel() *InputModal { b.noCancel = true; return b }

// Scrimmed fades the backdrop. Reserved for the surfaces where there is
// nothing else to do until the modal is answered.
func (b *InputModal) Scrimmed() *InputModal { b.scrim = true; return b }

// Open builds the modal and shows it.
func (b *InputModal) Open() *form {
	// SPLIT THE ROWS FIRST, and keep a field row's position among the FIELDS
	// rather than among the arguments. formValues is indexed by control, so a
	// prose row between two inputs would otherwise shift every later input's
	// answer by one — the second field would be validated against the third
	// field's rules and bound to the third field's variable, with nothing on
	// screen to say so.
	fields := make([]formField, 0, len(b.inputs))
	// chrome[k] is what renders immediately BEFORE the k-th field, so the
	// rows come out in the order the caller wrote them.
	chrome := map[int][]tui.Component{}
	// fieldOf[k] is the k-th field's input; validation and commit walk THIS,
	// not b.inputs.
	fieldOf := make([]Input, 0, len(b.inputs))
	for _, in := range b.inputs {
		f, ok := in.field()
		if !ok {
			if c := in.chrome(); c != nil {
				chrome[len(fields)] = append(chrome[len(fields)], c)
			}
			continue
		}
		fields = append(fields, f)
		fieldOf = append(fieldOf, in)
	}
	return b.m.openFormOpts(b.title, fields, func(v formValues) (bool, string) {
		// VALIDATE EVERY ROW BEFORE WRITING ANY BINDING. Writing as we go
		// would leave the caller's struct half-updated when a later row is
		// refused, and the operator would be looking at a modal that is still
		// open over data that has already changed.
		for i, in := range fieldOf {
			if err := in.validate(v, i); err != nil {
				return false, fmt.Sprintf("%s: %v", labelOf(in), err)
			}
		}
		for i, in := range fieldOf {
			in.commit(v, i)
		}
		if b.submit == nil {
			return true, ""
		}
		if err := b.submit(ModalResponse{Status: StatusSubmitted, Values: v}); err != nil {
			return false, err.Error()
		}
		return true, ""
	}, formOpts{
		scrim:      b.scrim,
		chrome:     chrome,
		okText:     b.okText,
		cancelText: b.cancelText,
		okMnemonic: b.okMnemonic,
		noCancel:   b.noCancel,
		onCancel: func(v formValues) {
			if b.cancel == nil {
				return
			}
			b.cancel(ModalResponse{Status: StatusCancelled, Values: v})
		},
	})
}

// labelOf names a row in a validation message, so the operator is told WHICH
// field is wrong rather than only that something is.
func labelOf(in Input) string {
	switch t := in.(type) {
	case *textInputSpec:
		return t.label
	case *selectInputSpec:
		return t.label
	}
	return "input"
}

// --- the two shapes that have names -------------------------------------------

// NewConfirmModal asks a yes/no question.
//
// A THIN WRAPPER, NOT A SECOND MODAL. A confirmation is an input modal with no
// inputs, so it is spelled as one: the buttons, the dismissal semantics and the
// cancel routing are the ones every other modal gets, and there is no second
// implementation that can drift from them. What the name buys is that the call
// site reads as the question it is asking.
//
// The button texts are the defaults — "OK" and "Cancel" — which is what a
// confirmation wants; WithOkText renames the affirmative for the cases where
// naming the act ("Delete", "Sign out") is clearer than agreeing to one.
//
// A confirm modal contributes NO fields, so nothing takes the keyboard ahead
// of the buttons and widget.Modal's own seeding stands: focus opens on the
// affirmative. A question whose yes is destructive should say so in the
// question and in WithOkText, because Enter reaches yes first.
func NewConfirmModal(m *Model, title, question string) *InputModal {
	return NewInputModal(m, title, NewTextValue(question))
}

// NewPromptModal asks for one line of text — the rename case.
//
// bind receives the new value only on submit, and only after validation, so a
// cancelled rename leaves the caller's variable holding what it held before.
func NewPromptModal(m *Model, title, label, old string, bind *string) *InputModal {
	return NewInputModal(m, title, NewTextInput(label, bind).WithOldValue(old).Required())
}
