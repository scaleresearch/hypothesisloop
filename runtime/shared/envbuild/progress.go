package envbuild

import (
	"fmt"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/opencontainers/go-digest"
)

// transferInterval is the least time between two reports of how far a build's transfers are.
// They stream many times a second for every layer pulled or pushed.
const transferInterval = 15 * time.Second

// steps follows the statuses of a build's solves and tells progress each step once as it starts
// and once as it ends, and the latest transfer at most every transferInterval.
type steps struct {
	progress     Progress
	names        map[digest.Digest]string
	started      map[digest.Digest]bool
	ended        map[digest.Digest]bool
	lastTransfer time.Time
}

func newSteps(progress Progress) *steps {
	return &steps{progress: progress, names: map[digest.Digest]string{}, started: map[digest.Digest]bool{}, ended: map[digest.Digest]bool{}}
}

func (s *steps) follow(status *client.SolveStatus) {
	for _, v := range status.Vertexes {
		s.names[v.Digest] = v.Name
		switch {
		case v.Completed != nil && !s.ended[v.Digest]:
			s.ended[v.Digest] = true
			switch {
			case v.Error != "":
				s.progress("%s: failed", v.Name)
			case v.Cached:
				s.progress("%s: cached", v.Name)
			case v.Started != nil:
				s.progress("%s: done in %s", v.Name, v.Completed.Sub(*v.Started).Round(time.Second))
			default:
				s.progress("%s: done", v.Name)
			}
		case v.Started != nil && v.Completed == nil && !s.started[v.Digest]:
			s.started[v.Digest] = true
			s.progress("%s: started", v.Name)
		}
	}
	if len(status.Statuses) == 0 || time.Since(s.lastTransfer) < transferInterval {
		return
	}
	s.lastTransfer = time.Now()
	latest := status.Statuses[len(status.Statuses)-1]
	s.progress("%s: %s %s", s.names[latest.Vertex], latest.ID, megabytes(latest.Current, latest.Total))
}

func megabytes(current, total int64) string {
	if total == 0 {
		return fmt.Sprintf("%.0f MB", float64(current)/1e6)
	}
	return fmt.Sprintf("%.0f/%.0f MB", float64(current)/1e6, float64(total)/1e6)
}
