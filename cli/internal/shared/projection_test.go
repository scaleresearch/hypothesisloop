package shared

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// Every field the CLI reads of an answer is one the API documents, of a type it decodes, so a
// field renamed or retyped on the platform fails here and not in an agent's hands.
func TestTheCLIReadsOnlyFieldsTheAPIAnswers(t *testing.T) {
	d := clitest.PublishedDocument(t)
	job := d.Answer(t, "GET", "/experiments/{id}")
	hypothesis := d.Answer(t, "GET", "/hypotheses/{id}")
	for _, c := range []struct {
		name string
		typ  reflect.Type
		in   *clitest.Schema
	}{
		{"jobBrief", reflect.TypeFor[jobBrief](), job},
		{"briefMetrics", reflect.TypeFor[briefMetrics](), d.Property(t, job, "metrics")},
		{"machineTag", reflect.TypeFor[machineTag](), d.Property(t, job, "ran_on")},
		{"jobResult", reflect.TypeFor[jobResult](), d.Items(t, d.Property(t, hypothesis, "jobs"))},
		{"hypothesisBrief", reflect.TypeFor[hypothesisBrief](), hypothesis},
		{"metricPoint", reflect.TypeFor[metricPoint](), d.Items(t, d.Answer(t, "GET", "/experiments/{id}/metrics"))},
	} {
		t.Run(c.name, func(t *testing.T) { d.Project(t, c.name, c.typ, c.in) })
	}
}
