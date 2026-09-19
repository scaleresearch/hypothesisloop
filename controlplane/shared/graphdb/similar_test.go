package graphdb

import "testing"

// Texts from the dummy demo agent runs: a reworded duplicate and a seed restated as its own child
// must read alike, the other hypotheses beside them must not.
func TestSimilaritiesFlagRewordedDuplicatesOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		docs []string
		dup  int
	}{
		{
			name: "sibling registered twice",
			text: "Pushing batch size lower still (BATCH_SIZE=4, 8x more gradient updates/epoch than baseline) raises val_accuracy further than BATCH_SIZE=8, since the confirmed bs8 win tracks update-count, not batch size per se -- unless per-step gradient noise at m=4 starts to hurt instead.",
			docs: []string{
				"Optimization limits val_accuracy: 40 epochs at LR 0.05 stop before convergence, so a better LR/epoch schedule raises it with the model unchanged.",
				"Model capacity limits val_accuracy: the 16-unit single-layer baseline underfits the curved two-moons boundary, so a larger MLP raises it.",
				"Training data limits val_accuracy: 640 training samples at NOISE 0.25 under-sample the boundary, so more samples raise it more than any model change does.",
				"BATCH_SIZE=8 (vs baseline 32) with LR unchanged reaches higher val_accuracy at the same epoch budget: 4x more gradient updates per epoch compensates for the low LR without changing the model.",
				"Adding pure-noise feature columns (N_FEATURES 2->10, 8 extra noise dims) hurts val_accuracy versus the 2-feature baseline, because the MLP's first-layer weights must now also average out 8 uninformative dimensions with no added signal.",
				"L2=0.001 regularization raises val_accuracy over the unregularized baseline by curbing overfitting to the 640-sample noisy training set.",
				"BATCH_SIZE=4 pushes further than BATCH_SIZE=8: 8x more gradient updates/epoch than baseline should continue the trend of BATCH_SIZE=8 beating BATCH_SIZE=32, within the same 40-epoch budget.",
			},
			dup: 6,
		},
		{
			name: "seed restated as its child",
			text: "Two hidden layers (32,16) beat both the 16-unit baseline and a wider single layer (64) at comparable or lower unit budget, because composing two nonlinear stages lets the network fit the two interleaved moons' curvature with fewer total activations than one wide layer needs (representational/compositional mechanism, not just more parameters).",
			docs: []string{
				"Model capacity limits val_accuracy: the 16-unit single-layer baseline underfits the curved two-moons boundary, so a larger MLP raises it.",
				"At a similar parameter count, two hidden layers (e.g. 32,16) beat one wider layer, because the boundary needs two composed bends.",
				"Optimization limits val_accuracy: 40 epochs at LR 0.05 stop before convergence, so a better LR/epoch schedule raises it with the model unchanged.",
				"Training data limits val_accuracy: 640 training samples at NOISE 0.25 under-sample the boundary, so more samples raise it more than any model change does.",
				"Widening the single hidden layer from 16 to 64 units (same depth) raises val_accuracy: more units per layer gives the sigmoid-output MLP more capacity to bend its single decision boundary around the two-moons curve, independent of any depth effect.",
			},
			dup: 1,
		},
	} {
		scores := similarities(tc.text, tc.docs)
		for i, score := range scores {
			if alike := score >= similarThreshold; alike != (i == tc.dup) {
				t.Errorf("%s: doc %d scored %.3f, want alike=%v", tc.name, i, score, i == tc.dup)
			}
		}
	}
}

func TestSimilaritiesIgnoreCaseStopWordsAndEmptyText(t *testing.T) {
	scores := similarities("The LR of 0.2 beats the baseline", []string{"lr 0.2 BEATS baseline", "", "the of a"})
	if scores[0] < 0.99 || scores[1] != 0 || scores[2] != 0 {
		t.Fatalf("scores = %v, want [~1 0 0]", scores)
	}
}
