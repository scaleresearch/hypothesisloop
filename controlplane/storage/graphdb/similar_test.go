package graphdb

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

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

// The first hypotheses of an experiment share its vocabulary whatever they claim, so in a graph of
// one or two a claim reads alike only to its own restatement, never to one that moves another lever.
func TestSimilaritiesTellLeversApartInATinyExperiment(t *testing.T) {
	const (
		workers      = "Holding chunk size fixed at the baseline, raising num_workers from 1 to 8 raises throughput."
		chunks       = "Holding num_workers fixed, a larger chunk size raises throughput."
		workersAtLen = "With chunk size fixed at the baseline of 64 rows, raising num_workers from 1 to 8 raises throughput in rows per second, because decoding is CPU bound and parallelises across cores until the disk saturates."
		chunksAtLen  = "With num_workers pinned at the baseline of 1, growing the chunk size from 64 to 1024 rows raises throughput in rows per second, because each read amortises the per-request overhead over more rows."
	)
	for _, tc := range []struct {
		name  string
		text  string
		docs  []string
		alike []bool
	}{
		{"another lever", chunks, []string{workers}, []bool{false}},
		{"another lever, the other way round", workers, []string{chunks}, []bool{false}},
		{"another lever, with its mechanism", chunksAtLen, []string{workersAtLen}, []bool{false}},
		{"restated", "With chunk size held at the baseline, raising num_workers from 1 to 8 raises throughput.", []string{workers}, []bool{true}},
		{"restated in another order", "Raising num_workers from 1 to 8 raises throughput while chunk size stays fixed at the baseline.", []string{workers}, []bool{true}},
		{"the first restated beside the second", "Throughput rises when num_workers goes from 1 to 8 and chunk size is fixed at the baseline.", []string{workers, chunks}, []bool{true, false}},
		{"the second restated beside the first", "A larger chunk size raises throughput when num_workers is held fixed.", []string{workers, chunks}, []bool{false, true}},
	} {
		for i, score := range similarities(tc.text, tc.docs) {
			if alike := score >= similarThreshold; alike != tc.alike[i] {
				t.Errorf("%s: doc %d scored %.3f, want alike=%v", tc.name, i, score, tc.alike[i])
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

// Texts of a sandboxed run: a researcher opened a new line restating another agent's confirmed one in
// other words (SoA is the channel-major layout) and with a stronger coder. At length it scored
// under similarThreshold against it, and must still read close as a new line; the other agent's
// refinements of that line, which change what is coded and how, must not.
func TestSimilaritiesHoldANewLineThatRestatesAnotherInOtherWords(t *testing.T) {
	const (
		layout      = `Structural claim: reorganizing each topic's point data from the control's point-major (AoS) byte layout into a channel-major (SoA) layout -- concatenating each PointCloud2 field's bytes across all points of a message before moving to the next field, in fixed field order, per topic -- exposes per-channel homogeneity (e.g. a run of same-width ring/t/range/signal values) that a byte-oriented compressor cannot see when fields of different types and scales are interleaved byte-by-byte per point. Coding step held identical to the control (zlib level 9) so the only variable is the structural layout. Expect compression_ratio to beat the control's 1.5724 purely from this reordering, since general-purpose compressors exploit local repetition and interleaved mixed-type fields destroy that locality (same principle as why columnar formats like Parquet/ORC compress far better than row-major CSV/JSON for the same data).`
		perChannel  = `Refines the channel-major (SoA) hypothesis: code each channel independently (its own zlib stream instead of one zlib pass over every field concatenated) and let each channel pick whichever of two transforms compresses smaller -- plain bytes, or consecutive xor-delta (point i XOR point i-1) which turns smoothly-varying channels (adjacent azimuth/ring samples are spatially close, so e.g. t, range, x/y/z share leading bytes) into many leading-zero bytes a byte-oriented compressor run-lengths away. The choice is per channel (1 flag byte each), so a channel xor-delta would hurt (e.g. label, intensity, reflectivity, ring) just keeps plain -- this is the 'separating each channel into its own stream coded by whatever fits that channel' structural claim, not a tuning change to the coder itself (still zlib level 9 throughout). Local test: raw_bytes=75,614,788 (matches canonical extraction), encoded_bytes=35,263,301, compression_ratio=2.1443 (+36.3% over control's 1.5724, +9.4% over the parent SoA hypothesis's 1.9598), lossless_correct=1, ~37s wall. Per-field dry run showed xor-delta wins on x/y/z/t/range/signal/near_ir (smoothly varying, high byte-entropy fields) and loses on ring/reflectivity/intensity/label (already low-entropy or non-monotonic), confirming the per-channel choice is doing real work rather than always picking one transform.`
		coderChoice = `Extends the per-channel transform-choice hypothesis with a third structural axis: per-channel entropy coder choice, not just per-channel transform choice. Each channel now picks the smallest of 4 candidates -- {plain, xor-delta} x {zlib level 9, lzma preset 9} -- instead of only choosing between zlib-plain and zlib-xor-delta. lzma's larger dictionary/context model (vs DEFLATE's 32KB window) is expected to win on the smooth, high-byte-entropy channels (x, y, z, t, range, signal, near_ir) where its longer-range modelling finds structure DEFLATE can't, while zlib keeps winning on small/low-entropy channels (ring, label) where lzma's container overhead doesn't pay off. Still fully lossless; the coder choice is just another structural degree of freedom in how each channel is coded, not a coder-internal tuning knob (both coders run at their respective 'best ratio' preset throughout, held fixed). Local test: raw_bytes=75,614,788 (matches canonical extraction), encoded_bytes=29,981,389, compression_ratio=2.5221 (+17.6% over the parent's 2.1443, +60.4% over the control's 1.5724), lossless_correct=1, ~100s wall (two coders x two transforms x 14 channels = 56 compressions during encode, decode fast).`
		restated    = `Splitting each topic's declared point-data fields into separate per-field byte streams (Structure-of-Arrays instead of the control's per-point AoS interleaving), then compressing the whole concatenated SoA buffer with LZMA (same general-purpose LZ family as the control's zlib, stronger entropy stage), will raise compression_ratio well above the 1.57242 zlib-on-AoS control. Rationale: interleaving unrelated fields (float32 xyz next to uint16 ring next to uint32 range, every point) dilutes each field's own local statistics for the compressor; grouping each field contiguously lets LZMA exploit per-field regularities (near-constant ring, near-linear t, smooth range/reflectivity/near_ir) that AoS hides. No point-cloud geometry assumed, so it generalizes safely and stays exactly lossless (round-trip verified in-process before reporting).`
	)
	pe := "pe-1"
	newLine := NewHypothesis{PlatformExperimentID: pe, Links: onRoot(pe)}
	refinement := NewHypothesis{PlatformExperimentID: pe, Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "h-1"}}}
	if closeAt(refinement) != similarThreshold || closeAt(newLine) != newLineThreshold {
		t.Fatalf("closeAt: refinement %v, new line %v", closeAt(refinement), closeAt(newLine))
	}
	scores := similarities(restated, []string{layout, perChannel, coderChoice})
	if scores[0] >= similarThreshold || scores[0] < closeAt(newLine) {
		t.Errorf("restated line scored %.3f against the one it restates, want in [%v, %v)", scores[0], closeAt(newLine), similarThreshold)
	}
	for i, score := range scores[1:] {
		if score >= closeAt(newLine) {
			t.Errorf("restated line scored %.3f against refinement %d, want under %v", score, i+1, closeAt(newLine))
		}
	}
}
