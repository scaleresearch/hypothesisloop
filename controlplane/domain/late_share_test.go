package domain

import (
	"math"
	"testing"
)

func TestLateShareIsAnEvenPartOfWhatIsUnspentTakenFirstFromTheResearchersThatEnded(t *testing.T) {
	for name, c := range map[string]struct {
		quotas []ResearcherQuota
		share  float64
		taken  map[string]float64
	}{
		"one ended with plenty left": {
			quotas: []ResearcherQuota{
				{AgentID: "a", Guaranteed: 10, Used: 4},
				{AgentID: "b", Guaranteed: 10, Used: 2, Ended: true},
			},
			// 6 + 8 unspent over the one at work and the newcomer.
			share: 7, taken: map[string]float64{"b": 7},
		},
		"the researchers at work give the rest of an even part": {
			quotas: []ResearcherQuota{
				{AgentID: "a", Guaranteed: 10},
				{AgentID: "b", Guaranteed: 10},
				{AgentID: "c", Guaranteed: 10, Used: 9, Ended: true},
			},
			// 21 unspent over three: c gives its 1, a and b the 6 left evenly.
			share: 7, taken: map[string]float64{"c": 1, "a": 3, "b": 3},
		},
		"a researcher at work below the share gives nothing": {
			quotas: []ResearcherQuota{
				{AgentID: "a", Guaranteed: 10, Used: 9},
				{AgentID: "b", Guaranteed: 10, Used: 1},
				{AgentID: "c", Guaranteed: 10, Used: 6},
			},
			// 1 + 9 + 4 over four: b and c give in proportion to the 5.5 and 0.5 above it.
			share: 3.5, taken: map[string]float64{"b": 3.208333, "c": 0.291667},
		},
		"taken across several in order": {
			quotas: []ResearcherQuota{
				{AgentID: "b", Guaranteed: 10, Used: 7, Ended: true},
				{AgentID: "c", Burst: 10, Used: 5, Ended: true},
			},
			share: 8, taken: map[string]float64{"b": 3, "c": 5},
		},
		"nobody ended": {
			quotas: []ResearcherQuota{{AgentID: "a", Guaranteed: 10, Used: 4}},
			share:  3, taken: map[string]float64{"a": 3},
		},
		"an ended one that overspent gives nothing": {
			quotas: []ResearcherQuota{{AgentID: "b", Guaranteed: 10, Used: 12, Ended: true}},
			share:  0, taken: map[string]float64{},
		},
	} {
		share, taken := LateShare(c.quotas)
		if math.Abs(share-c.share) > 1e-9 || len(taken) != len(c.taken) {
			t.Errorf("%s: got = %g %v, want = %g %v", name, share, taken, c.share, c.taken)
			continue
		}
		for id, want := range c.taken {
			if math.Abs(taken[id]-want) > 1e-6 {
				t.Errorf("%s: taken from %s got = %g, want = %g", name, id, taken[id], want)
			}
		}
	}
}
