package k8sexec

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExistingJobMatchesDesiredRequiresHashAndIdentity(t *testing.T) {
	desired := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{DesiredSpecHashAnnotation: "wanted"}}}
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Labels:      map[string]string{"hypothesisloop.io/managed-by": "hypothesisloop", "hypothesisloop.io/experiment-id": "exp-1"},
		Annotations: map[string]string{DesiredSpecHashAnnotation: "stale"},
	}}
	if existingJobMatchesDesired(existing, desired, "exp-1") {
		t.Fatal("mismatched desired hash was accepted")
	}
	existing.Annotations[DesiredSpecHashAnnotation] = "wanted"
	delete(existing.Labels, "hypothesisloop.io/experiment-id")
	if existingJobMatchesDesired(existing, desired, "exp-1") {
		t.Fatal("missing experiment identity was accepted")
	}
}
