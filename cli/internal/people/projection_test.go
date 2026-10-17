package people

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// Every field the CLI reads of an answer is one the API documents, of a type it decodes, so a
// field renamed or retyped on the platform fails here and not in an agent's hands.
func TestTheCLIReadsOnlyFieldsTheAPIAnswers(t *testing.T) {
	d := clitest.PublishedDocument(t)
	t.Run("registeredCluster", func(t *testing.T) {
		d.Project(t, "registeredCluster", reflect.TypeFor[registeredCluster](), d.Answer(t, "POST", "/orgs/{id}/clusters"))
	})
}
