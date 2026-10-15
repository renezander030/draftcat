package main

import (
	"errors"
	"strings"

	"github.com/renezander030/draftcat/internal/config"
)

// haltedRunError is a pipeline error that also knows where the run stopped.
// Its message is the underlying error's, unchanged, so stored run errors and
// logs read as before; haltNote adds the halt to operator notices.
type haltedRunError struct {
	err    error
	step   string
	notRun []string
}

func (e *haltedRunError) Error() string { return e.err.Error() }
func (e *haltedRunError) Unwrap() error { return e.err }

// haltedRun records that the run stopped in steps[index] and that every later
// step did not run.
func haltedRun(err error, steps []config.StepConfig, index int) error {
	if index < 0 || index >= len(steps) {
		return err
	}
	h := &haltedRunError{err: err, step: steps[index].Name}
	for _, st := range steps[index+1:] {
		h.notRun = append(h.notRun, st.Name)
	}
	return h
}

// haltNote names the step a failed run stopped in and the steps that never
// ran, for the operator: those are the actions that did not happen. Empty for
// any other error.
func haltNote(err error) string {
	var h *haltedRunError
	if !errors.As(err, &h) {
		return ""
	}
	note := "\nhalted at: " + h.step
	if len(h.notRun) == 0 {
		return note + " (last step)"
	}
	return note + "\nnot run: " + strings.Join(h.notRun, ", ")
}
