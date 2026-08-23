package db

import (
	"strings"
	"testing"
)

// The checkpoint-window grant is derived from this query, and a preemption clears cluster_name in
// the same UPDATE that records the termination (RequeuePreempted: the job holds no capacity any
// more, so the next admission tick may place it anywhere). Narrowing this read by cluster_name
// therefore asked a row that had just forgotten its cluster which cluster it was on, matched
// nothing, and left the grant list permanently empty — so every preempted job was killed on the
// ordinary 5s shutdown grace and lost the checkpoint the requeue's rescaled estimate assumes it
// wrote.
func TestRecentlyUndesiredWorkloadsIsNotNarrowedByAClusterTheTerminationJustCleared(t *testing.T) {
	if strings.Contains(recentlyUndesiredWorkloadsQuery, "cluster_name =") {
		t.Fatalf("query narrows by cluster_name, which a termination clears in the same write: %s", recentlyUndesiredWorkloadsQuery)
	}
}

// The answer has to expire on its own, which is the entire reason there is no terminating state
// to write, advance or clear. Without the time bound every experiment ever finished reads as
// "recently undesired" and a grant would outlive the window it grants.
func TestRecentlyUndesiredWorkloadsIsBoundedByStatusAndTime(t *testing.T) {
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "status <> ALL($1)") {
		t.Fatalf("query does not exclude the desired statuses: %s", recentlyUndesiredWorkloadsQuery)
	}
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "updated_at > now() - $2::interval") {
		t.Fatalf("query is not time-bounded, so a grant would never expire: %s", recentlyUndesiredWorkloadsQuery)
	}
}
