package domain

import (
	"math"
	"testing"
)

func TestLateShareIsAnEvenPartOfWhatIsUnspentTakenFromTheResearchersThatEnded(t *testing.T) {
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
		"never more than the ended left": {
			quotas: []ResearcherQuota{
				{AgentID: "a", Guaranteed: 10},
				{AgentID: "b", Guaranteed: 10},
				{AgentID: "c", Guaranteed: 10, Used: 9, Ended: true},
			},
			share: 1, taken: map[string]float64{"c": 1},
		},
		"taken across several in order": {
			quotas: []ResearcherQuota{
				{AgentID: "b", Guaranteed: 10, Used: 7, Ended: true},
				{AgentID: "c", Burst: 10, Used: 5, Ended: true},
			},
			share: 8, taken: map[string]float64{"b": 3, "c": 5},
		},
		"nobody ended": {
			quotas: []ResearcherQuota{{AgentID: "a", Guaranteed: 10}},
			share:  0, taken: map[string]float64{},
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
			if math.Abs(taken[id]-want) > 1e-9 {
				t.Errorf("%s: taken from %s got = %g, want = %g", name, id, taken[id], want)
			}
		}
	}
}
