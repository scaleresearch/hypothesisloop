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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	// similarThreshold is the TF-IDF cosine at which two hypotheses read alike. Chosen on the
	// dummy demo agent runs: reworded duplicates scored 0.37 to 0.45.
	similarThreshold = 0.35
	maxSimilar       = 5
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

// similarities returns the TF-IDF cosine between text and each of docs, with document frequencies
// taken over docs and text together.
func similarities(text string, docs []string) []float64 {
	all := append(slices.Clone(docs), text)
	df := map[string]int{}
	counts := make([]map[string]float64, len(all))
	for i, d := range all {
		counts[i] = map[string]float64{}
		for _, w := range words(d) {
			if counts[i][w] == 0 {
				df[w]++
			}
			counts[i][w]++
		}
	}
	n := float64(len(all))
	vecs := make([]map[string]float64, len(all))
	for i, c := range counts {
		var norm float64
		for w, tf := range c {
			c[w] = tf * (math.Log((1+n)/(1+float64(df[w]))) + 1)
			norm += c[w] * c[w]
		}
		norm = math.Sqrt(norm)
		for w := range c {
			c[w] /= norm
		}
		vecs[i] = c
	}
	query := vecs[len(all)-1]
	out := make([]float64, len(docs))
	for i := range docs {
		for w, x := range query {
			out[i] += x * vecs[i][w]
		}
	}
	return out
}

// untoldSimilar returns the hypotheses of n's platform experiment that n must be told apart from:
// those whose text reads close to n's, except the ones n builds on, which a refinement is expected
// to read close to, and those n builds on that are refuted, minus every one n names in
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
RETURN h.id, h.agent_id, coalesce(h.actor_kind, 'unverified'), coalesce(h.actor_id, ''), h.author, h.status, h.text, collect(t.id)`, map[string]any{"pe": n.PlatformExperimentID})
	if err != nil {
		return nil, err
	}
	var d decoder
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = d.str(r.Values[6])
	}
	scores := similarities(n.Text, texts)
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
		case scores[i] >= similarThreshold && !parents[h.ID]:
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
