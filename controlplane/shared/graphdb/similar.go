package graphdb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ErrSimilar is a registration refused because it reads close to existing hypotheses the caller
// has not told it apart from. The error is a *SimilarError carrying them.
var ErrSimilar = errors.New("reads close to existing hypotheses")

// SimilarError lists the hypotheses a registration must be told apart from before it is created.
type SimilarError struct {
	Similar []domain.SimilarHypothesis
}

func (e *SimilarError) Error() string {
	return fmt.Sprintf("%s: %d not yet told apart", ErrSimilar, len(e.Similar))
}

func (e *SimilarError) Unwrap() error { return ErrSimilar }

const (
	// similarThreshold is the score of similarities at which two hypotheses read alike. Chosen on
	// the dummy demo agent runs: reworded duplicates scored 0.069 and 0.107, the closest other
	// hypothesis 0.021.
	similarThreshold = 0.05
	// newLineThreshold is that score for a hypothesis that builds on the objective alone: a new
	// line, placed under nothing its agent had to read first. Agents state a claim at length, with
	// its rationale and numbers, which dilutes the score: on a hosted run a new line restating
	// another agent's confirmed one in other words scored 0.027 against it. No bar tells that apart
	// from a new line that moves another lever and explains how (0.036 in the tests), so this one
	// holds both: a distinct line passes with one distinct_from, a duplicated one costs a line of work.
	newLineThreshold = 0.02
	maxSimilar       = 5
	// pairReach is how many following words a word is paired with.
	pairReach = 2
)

var (
	wordPattern = regexp.MustCompile(`[a-z0-9_]+(?:\.[0-9]+)?`)
	stopWords   = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields("a an the and or of to in on at by for with from as is are be been was were it its " +
		"this that these those than then so not no but if into over under per vs via same more most less does do") {
		stopWords[w] = true
	}
}

func words(text string) []string {
	var out []string
	for _, w := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		if !stopWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// pairs counts each word of text followed within pairReach words by another, in that order. Two
// claims that change different levers of one experiment use the same words and differ in which
// word goes with which, so the pairs tell them apart where the words alone cannot.
func pairs(text string) map[string]float64 {
	ws := words(text)
	out := map[string]float64{}
	for i, w := range ws {
		for _, next := range ws[i+1 : min(i+1+pairReach, len(ws))] {
			out[w+" "+next]++
		}
	}
	return out
}

// similarities returns the cosine between the word pairs of text and of each of docs. A pair found
// in df of the n texts weighs log((n+1)/df), so what every text says, the experiment's own
// vocabulary, weighs least at any n, two included. A floor under that weight makes the first
// hypotheses of an experiment all read alike.
func similarities(text string, docs []string) []float64 {
	all := append(slices.Clone(docs), text)
	df := map[string]float64{}
	vecs := make([]map[string]float64, len(all))
	for i, d := range all {
		vecs[i] = pairs(d)
		for p := range vecs[i] {
			df[p]++
		}
	}
	norms := make([]float64, len(all))
	for i, v := range vecs {
		for p, count := range v {
			v[p] = count * math.Log(float64(len(all)+1)/df[p])
			norms[i] += v[p] * v[p]
		}
	}
	query := len(all) - 1
	out := make([]float64, len(docs))
	for i := range docs {
		if norms[i] == 0 || norms[query] == 0 {
			continue
		}
		for p, x := range vecs[query] {
			out[i] += x * vecs[i][p]
		}
		out[i] /= math.Sqrt(norms[i] * norms[query])
	}
	return out
}

// closeAt is the score at which n reads close to another hypothesis: newLineThreshold when n
// builds on the objective alone, else similarThreshold.
func closeAt(n NewHypothesis) float64 {
	for _, l := range n.Links {
		if l.Type == domain.LinkBuildsOn && l.To != n.PlatformExperimentID {
			return similarThreshold
		}
	}
	return newLineThreshold
}

// untoldSimilar returns the hypotheses of n's platform experiment that n must be told apart from:
// those whose text reads close to n's (closeAt), except the ones n builds on, which a refinement is
// expected to read close to, and those n builds on that are refuted, minus every one n names in
// DistinctFrom or in a contradicts or related link. Refuted parents first, then closest first, at
// most maxSimilar.
func untoldSimilar(ctx context.Context, tx neo4j.ManagedTransaction, n NewHypothesis) ([]domain.SimilarHypothesis, error) {
	told := map[string]bool{}
	parents := map[string]bool{}
	for _, id := range n.DistinctFrom {
		told[id] = true
	}
	for _, l := range n.Links {
		if l.Type == domain.LinkBuildsOn {
			parents[l.To] = true
		} else {
			told[l.To] = true
		}
	}
	records, err := collect(ctx, tx, `MATCH (h:Hypothesis {platform_experiment_id: $pe})
OPTIONAL MATCH (h)-[:BUILDS_ON]->(t)
RETURN h.id, h.agent_id, coalesce(h.actor_kind, 'unverified'), coalesce(h.actor_id, ''), coalesce(h.author, ''), h.status, h.text, collect(t.id)`, map[string]any{"pe": n.PlatformExperimentID})
	if err != nil {
		return nil, err
	}
	var d decoder
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = d.str(r.Values[6])
	}
	scores := similarities(n.Text, texts)
	threshold := closeAt(n)
	type scored struct {
		h     domain.SimilarHypothesis
		score float64
	}
	var found []scored
	for i, r := range records {
		h := domain.SimilarHypothesis{ID: d.str(r.Values[0]), AgentID: d.str(r.Values[1]),
			Status: domain.HypothesisStatus(d.str(r.Values[5])), Text: texts[i], BuildsOn: []string{}}
		h.Actor = domain.StoredActor(domain.ActorKind(d.str(r.Values[2])), d.str(r.Values[3]), d.str(r.Values[4]), h.AgentID)
		if told[h.ID] {
			continue
		}
		switch {
		case parents[h.ID] && h.Status == domain.HypothesisRefuted:
			h.Reason = "refuted_parent"
		case scores[i] >= threshold && !parents[h.ID]:
			h.Reason = "close_text"
		default:
			continue
		}
		for _, id := range r.Values[7].([]any) {
			h.BuildsOn = append(h.BuildsOn, d.str(id))
		}
		found = append(found, scored{h, scores[i]})
	}
	if d.err != nil {
		return nil, d.err
	}
	sort.Slice(found, func(a, b int) bool {
		if ra, rb := found[a].h.Reason == "refuted_parent", found[b].h.Reason == "refuted_parent"; ra != rb {
			return ra
		}
		return found[a].score > found[b].score
	})
	out := make([]domain.SimilarHypothesis, 0, min(len(found), maxSimilar))
	for _, f := range found[:min(len(found), maxSimilar)] {
		out = append(out, f.h)
	}
	return out, nil
}
