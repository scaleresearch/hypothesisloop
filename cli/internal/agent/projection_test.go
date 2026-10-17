package agent

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// Every field the CLI reads of an answer is one the API documents, of a type it decodes, so a
// field renamed or retyped on the platform fails here and not in an agent's hands.
func TestTheCLIReadsOnlyFieldsTheAPIAnswers(t *testing.T) {
	d := clitest.PublishedDocument(t)
	submit := d.Answer(t, "POST", "/experiments")
	for _, c := range []struct {
		name string
		typ  reflect.Type
		in   *clitest.Schema
	}{
		{"submitted", reflect.TypeFor[submitted](), submit},
		{"runBest", reflect.TypeFor[runBest](), d.Property(t, submit, "run_best")},
		{"declaredExperiment", reflect.TypeFor[declaredExperiment](), d.Answer(t, "GET", "/platform-experiments/{id}")},
	} {
		t.Run(c.name, func(t *testing.T) { d.Project(t, c.name, c.typ, c.in) })
	}
}
