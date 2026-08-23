package scheduler

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Queue order decides who gets scarce capacity, so the sort comparators are as load-bearing as the
// placement search — and they are far easier to get subtly wrong. sort.SliceStable requires its
// comparator to be a strict weak ordering; when it is not, Go does not complain, it just produces
// an order that depends on the input permutation. Since the input permutation is whatever order
// the database returned rows in, a broken comparator shows up in production as "the same queue
// admits different jobs on different ticks" — non-reproducible, and invisible to any test that
// checks one hand-built list.
//
// These tests assert the properties that actually protect against that, over randomized inputs:
// the result must not depend on input order, and sorting must be idempotent.

// sortKey is the ordering key each comparator claims to implement, as a vector. Two experiments
// with equal key vectors are genuinely tied and may legitimately appear in either order; two with
// different key vectors must always come out in the same relative order no matter how the input
// was shuffled. Comparing sorted *key sequences* rather than sorted IDs is what lets these tests
// tolerate real ties while still catching real ordering instability.
type sortKey struct {
	fields []float64
}

func (k sortKey) String() string { return fmt.Sprint(k.fields) }

func guaranteedKey(exp *domain.Experiment, quotaMap map[string]*domain.AgentQuota, completion map[string]float64, window time.Duration) sortKey {
	nilQueuedAt, bucket := 1.0, 0.0
	if exp.QueuedAt != nil {
		nilQueuedAt = 0
		bucket = float64(exp.QueuedAt.Truncate(window).UnixNano())
	}
	return sortKey{fields: []float64{
		nilQueuedAt,
		bucket,
		dominantUtilization(quotaMap, exp),
		-completionBucket(completion[exp.ID]),
		dominantCostFraction(quotaMap, exp),
		-exp.PriorityScore,
	}}
}

func burstKey(exp *domain.Experiment, quotaMap map[string]*domain.AgentQuota, completion map[string]float64) sortKey {
	// A missing queued_at sorts last, so it must model as larger than any real timestamp — not as
	// zero, which would claim it sorts first.
	queuedAt := math.Inf(1)
	if exp.QueuedAt != nil {
		queuedAt = float64(exp.QueuedAt.UnixNano())
	}
	return sortKey{fields: []float64{
		dominantUtilization(quotaMap, exp),
		-completionBucket(completion[exp.ID]),
		dominantCostFraction(quotaMap, exp),
		-exp.PriorityScore,
		queuedAt,
	}}
}

func keySequence(exps []*domain.Experiment, key func(*domain.Experiment) sortKey) string {
	out := ""
	for _, e := range exps {
		out += key(e).String() + "|"
	}
	return out
}

// generateQueue builds a random queue drawn from a deliberately coarse grid: few distinct
// priorities, few distinct quota rows, timestamps clustered inside and across the fairness window.
// Ties and near-ties are the interesting inputs — a comparator only misbehaves where it has to
// decide that two things are equivalent — so the generator manufactures them rather than leaving
// them to chance.
func generateQueue(rnd *rand.Rand, n int, window time.Duration) ([]*domain.Experiment, map[string]*domain.AgentQuota, map[string]float64) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	quotaMap := map[string]*domain.AgentQuota{}
	completion := map[string]float64{}
	exps := make([]*domain.Experiment, 0, n)

	for i := 0; i < n; i++ {
		agent := fmt.Sprintf("agent-%d", rnd.Intn(3))
		pe := fmt.Sprintf("pe-%d", rnd.Intn(2))
		id := fmt.Sprintf("exp-%02d", i)

		var queuedAt *time.Time
		if rnd.Intn(10) > 0 { // ~10% have no queued_at, exercising the nil branch
			// Offsets straddle the window boundary so some pairs share an age bucket and others
			// do not — the branch where dominant utilization takes over is only reachable when
			// two jobs land in the same bucket.
			t := base.Add(time.Duration(rnd.Intn(5)) * (window / 2))
			queuedAt = &t
		}
		key := quotaKey(agent, pe)
		if _, ok := quotaMap[key]; !ok && rnd.Intn(4) > 0 { // some jobs deliberately have no quota row
			quotaMap[key] = &domain.AgentQuota{
				GuaranteedAcceleratorHours: float64(rnd.Intn(3)) * 10,
				UsedGuaranteedAccH:         float64(rnd.Intn(4)) * 5,
			}
		}
		exp := &domain.Experiment{
			ID:                   id,
			AgentID:              agent,
			PlatformExperimentID: pe,
			QueuedAt:             queuedAt,
			PriorityScore:        float64(rnd.Intn(3)),
			EstimatedCostAccH:    float64(rnd.Intn(3)),
			CapacityTier:         domain.CapacityGuaranteed,
		}
		if rnd.Intn(2) == 0 {
			completion[id] = float64(rnd.Intn(5)) / 4
		}
		exps = append(exps, exp)
	}
	return exps, quotaMap, completion
}

func shuffled(rnd *rand.Rand, exps []*domain.Experiment) []*domain.Experiment {
	out := append([]*domain.Experiment(nil), exps...)
	rnd.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestSortGuaranteedIsIndependentOfInputOrder is the central anti-flake property: the admitted
// order must be a function of the jobs, not of the order the store happened to return them in.
func TestSortGuaranteedIsIndependentOfInputOrder(t *testing.T) {
	const window = 10 * time.Minute
	rnd := rand.New(rand.NewSource(20260901))

	for iteration := 0; iteration < 500; iteration++ {
		exps, quotaMap, completion := generateQueue(rnd, 2+rnd.Intn(9), window)
		key := func(e *domain.Experiment) sortKey { return guaranteedKey(e, quotaMap, completion, window) }

		reference := shuffled(rnd, exps)
		sortGuaranteed(reference, quotaMap, completion, window)
		want := keySequence(reference, key)

		// Several independent shuffles of the same multiset must all sort to the same key
		// sequence. One that does not is a comparator that is not a strict weak ordering.
		for attempt := 0; attempt < 8; attempt++ {
			candidate := shuffled(rnd, exps)
			sortGuaranteed(candidate, quotaMap, completion, window)
			if got := keySequence(candidate, key); got != want {
				t.Fatalf("iteration %d attempt %d: sortGuaranteed order depends on input order\n got: %s\nwant: %s\n%s",
					iteration, attempt, got, want, describeQueue(exps, quotaMap, completion))
			}
		}
	}
}

func TestSortBurstIsIndependentOfInputOrder(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260902))

	for iteration := 0; iteration < 500; iteration++ {
		exps, quotaMap, completion := generateQueue(rnd, 2+rnd.Intn(9), time.Minute)
		key := func(e *domain.Experiment) sortKey { return burstKey(e, quotaMap, completion) }

		reference := shuffled(rnd, exps)
		sortBurst(reference, quotaMap, completion)
		want := keySequence(reference, key)

		for attempt := 0; attempt < 8; attempt++ {
			candidate := shuffled(rnd, exps)
			sortBurst(candidate, quotaMap, completion)
			if got := keySequence(candidate, key); got != want {
				t.Fatalf("iteration %d attempt %d: sortBurst order depends on input order\n got: %s\nwant: %s\n%s",
					iteration, attempt, got, want, describeQueue(exps, quotaMap, completion))
			}
		}
	}
}

// TestSortsAreIdempotent guards the other half of the strict-weak-ordering contract. Re-sorting an
// already-sorted queue must change nothing; if it does, the comparator disagrees with itself and
// the queue can churn indefinitely across ticks that see identical state.
func TestSortsAreIdempotent(t *testing.T) {
	const window = 10 * time.Minute
	rnd := rand.New(rand.NewSource(20260903))

	for iteration := 0; iteration < 500; iteration++ {
		exps, quotaMap, completion := generateQueue(rnd, 2+rnd.Intn(9), window)

		guaranteed := shuffled(rnd, exps)
		sortGuaranteed(guaranteed, quotaMap, completion, window)
		once := keySequence(guaranteed, func(e *domain.Experiment) sortKey { return guaranteedKey(e, quotaMap, completion, window) })
		sortGuaranteed(guaranteed, quotaMap, completion, window)
		twice := keySequence(guaranteed, func(e *domain.Experiment) sortKey { return guaranteedKey(e, quotaMap, completion, window) })
		if once != twice {
			t.Fatalf("iteration %d: sortGuaranteed is not idempotent\nonce:  %s\ntwice: %s", iteration, once, twice)
		}

		burst := shuffled(rnd, exps)
		sortBurst(burst, quotaMap, completion)
		once = keySequence(burst, func(e *domain.Experiment) sortKey { return burstKey(e, quotaMap, completion) })
		sortBurst(burst, quotaMap, completion)
		twice = keySequence(burst, func(e *domain.Experiment) sortKey { return burstKey(e, quotaMap, completion) })
		if once != twice {
			t.Fatalf("iteration %d: sortBurst is not idempotent\nonce:  %s\ntwice: %s", iteration, once, twice)
		}
	}
}

// TestSortsPreserveTheQueue is the cheap but essential invariant: sorting must reorder the queue,
// never lose or duplicate a job. A dropped job is a job that silently never runs.
func TestSortsPreserveTheQueue(t *testing.T) {
	const window = 10 * time.Minute
	rnd := rand.New(rand.NewSource(20260904))

	for iteration := 0; iteration < 300; iteration++ {
		exps, quotaMap, completion := generateQueue(rnd, 1+rnd.Intn(10), window)
		want := idMultiset(exps)

		guaranteed := shuffled(rnd, exps)
		sortGuaranteed(guaranteed, quotaMap, completion, window)
		if got := idMultiset(guaranteed); got != want {
			t.Fatalf("iteration %d: sortGuaranteed changed the queue contents\n got: %v\nwant: %v", iteration, got, want)
		}

		burst := shuffled(rnd, exps)
		sortBurst(burst, quotaMap, completion)
		if got := idMultiset(burst); got != want {
			t.Fatalf("iteration %d: sortBurst changed the queue contents\n got: %v\nwant: %v", iteration, got, want)
		}
	}
}

// TestSortGuaranteedWithZeroFairnessWindowStaysStrictFIFO covers the branch the randomized tests
// above never take, since they always pass a positive window. With no window configured the sort
// must fall back to exact queued_at ordering rather than silently dropping the age criterion.
func TestSortGuaranteedWithZeroFairnessWindowStaysStrictFIFO(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	older, newer := base, base.Add(time.Hour)
	// The newer job belongs to a far less utilized agent, which would win on the in-bucket
	// fairness tiebreak. With no fairness window there are no buckets, so age must decide.
	quotaMap := map[string]*domain.AgentQuota{
		quotaKey("agent-busy", "pe-1"): {GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 9},
		quotaKey("agent-idle", "pe-1"): {GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 0},
	}
	exps := []*domain.Experiment{
		{ID: "new", AgentID: "agent-idle", PlatformExperimentID: "pe-1", QueuedAt: &newer, EstimatedCostAccH: 1},
		{ID: "old", AgentID: "agent-busy", PlatformExperimentID: "pe-1", QueuedAt: &older, EstimatedCostAccH: 1},
	}
	sortGuaranteed(exps, quotaMap, nil, 0)
	if exps[0].ID != "old" {
		t.Fatalf("order = [%s, %s], want the older job first: with no fairness window the sort is strict FIFO", exps[0].ID, exps[1].ID)
	}
}

// TestSortGuaranteedPutsMissingQueuedAtLast pins the deliberate choice documented in sortGuaranteed:
// a row with no queued_at sorts last, and does so deterministically. Treating it as "equal on age"
// to everything would break transitivity and make the whole queue order unstable.
func TestSortGuaranteedPutsMissingQueuedAtLast(t *testing.T) {
	const window = 10 * time.Minute
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	exps := []*domain.Experiment{
		{ID: "no-queued-at"},
		{ID: "queued", QueuedAt: &base},
	}
	sortGuaranteed(exps, nil, nil, window)
	if exps[len(exps)-1].ID != "no-queued-at" {
		t.Fatalf("order = [%s, %s], want the job with no queued_at last", exps[0].ID, exps[1].ID)
	}
}

// --- interleaveByAgent ----------------------------------------------------------------------

// TestInterleaveByAgentInvariants checks the three things the burst fairness rotation must never
// break, over randomized queue shapes: it must not lose or duplicate a job, it must not reorder
// any single agent's own jobs (those are already priority-sorted), and no agent may get a second
// slot before every agent with work left has had a first.
func TestInterleaveByAgentInvariants(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260905))

	for iteration := 0; iteration < 1000; iteration++ {
		// Deliberately lopsided: one agent with a deep queue against several with one job each is
		// exactly the shape interleaving exists to defend against.
		var exps []*domain.Experiment
		agentCount := 1 + rnd.Intn(4)
		for a := 0; a < agentCount; a++ {
			depth := 1 + rnd.Intn(5)
			for j := 0; j < depth; j++ {
				exps = append(exps, &domain.Experiment{
					ID:      fmt.Sprintf("a%d-j%d", a, j),
					AgentID: fmt.Sprintf("agent-%d", a),
				})
			}
		}
		input := shuffled(rnd, exps)
		before := idMultiset(input)
		inputOrderByAgent := orderByAgent(input)

		out := interleaveByAgent(input)

		if got := idMultiset(out); got != before {
			t.Fatalf("iteration %d: interleaveByAgent changed the queue contents\n got: %v\nwant: %v", iteration, got, before)
		}
		if got := orderByAgent(out); !sameOrder(got, inputOrderByAgent) {
			t.Fatalf("iteration %d: interleaveByAgent reordered an agent's own jobs\n got: %v\nwant: %v",
				iteration, got, inputOrderByAgent)
		}
		// Bounded lead: scanning the output, an agent's k-th job may not appear before some other
		// agent's (k-1)-th, when that agent still had jobs to give.
		seen := map[string]int{}
		for position, e := range out {
			seen[e.AgentID]++
			for agent, total := range inputOrderByAgent {
				if agent == e.AgentID {
					continue
				}
				if len(total) >= seen[e.AgentID]-1 && seen[agent] < seen[e.AgentID]-1 {
					t.Fatalf("iteration %d: at position %d agent %s has %d jobs admitted while agent %s (with %d queued) has only %d — the rotation let one agent get more than a one-job lead",
						iteration, position, e.AgentID, seen[e.AgentID], agent, len(total), seen[agent])
				}
			}
		}
	}
}

// TestInterleaveByAgentHandlesEmptyAndSingle covers the degenerate inputs the randomized test
// above never generates.
func TestInterleaveByAgentHandlesEmptyAndSingle(t *testing.T) {
	if got := interleaveByAgent(nil); len(got) != 0 {
		t.Fatalf("interleaveByAgent(nil) = %v, want empty", got)
	}
	if got := interleaveByAgent([]*domain.Experiment{}); len(got) != 0 {
		t.Fatalf("interleaveByAgent(empty) = %v, want empty", got)
	}
	one := []*domain.Experiment{{ID: "only", AgentID: "agent-a"}}
	if got := interleaveByAgent(one); len(got) != 1 || got[0].ID != "only" {
		t.Fatalf("interleaveByAgent(single) = %v, want the one job back", got)
	}
	// Every job belonging to one agent is the case where interleaving must be a no-op rather than
	// a reshuffle — there is nobody to interleave with.
	same := []*domain.Experiment{
		{ID: "j0", AgentID: "agent-a"}, {ID: "j1", AgentID: "agent-a"}, {ID: "j2", AgentID: "agent-a"},
	}
	got := interleaveByAgent(same)
	for i, e := range got {
		if e.ID != same[i].ID {
			t.Fatalf("interleaveByAgent reordered a single agent's queue: got %s at %d, want %s", e.ID, i, same[i].ID)
		}
	}
}

// --- shared helpers -------------------------------------------------------------------------

func idMultiset(exps []*domain.Experiment) string {
	ids := make([]string, 0, len(exps))
	for _, e := range exps {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	return fmt.Sprint(ids)
}

func orderByAgent(exps []*domain.Experiment) map[string][]string {
	out := map[string][]string{}
	for _, e := range exps {
		out[e.AgentID] = append(out[e.AgentID], e.ID)
	}
	return out
}

func sameOrder(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for agent, ids := range a {
		other, ok := b[agent]
		if !ok || len(other) != len(ids) {
			return false
		}
		for i := range ids {
			if ids[i] != other[i] {
				return false
			}
		}
	}
	return true
}

func describeQueue(exps []*domain.Experiment, quotaMap map[string]*domain.AgentQuota, completion map[string]float64) string {
	out := "queue:\n"
	for _, e := range exps {
		queuedAt := "nil"
		if e.QueuedAt != nil {
			queuedAt = e.QueuedAt.Format(time.RFC3339)
		}
		out += fmt.Sprintf("  %s agent=%s pe=%s queued=%s priority=%v cost=%v completion=%v util=%v\n",
			e.ID, e.AgentID, e.PlatformExperimentID, queuedAt, e.PriorityScore, e.EstimatedCostAccH,
			completion[e.ID], dominantUtilization(quotaMap, e))
	}
	return out
}
