package envbuild

import (
	"fmt"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/opencontainers/go-digest"
)

// transferInterval is the least time between two reports of how far a build's transfers are.
// They stream many times a second for every layer pulled or pushed.
const transferInterval = 15 * time.Second

// steps follows the statuses of a build's solves and tells progress each run of a step once as
// it starts and once as it ends, and the latest transfer at most every transferInterval. progress
// is told one line at a time, the load's among the solve's. A step the daemon shares with an
// earlier solve that ended it before since is told as reused; one that failed there, cancelled
// with that solve, runs again, a new run with its own start.
type steps struct {
	progress     Progress
	since        time.Time
	names        map[digest.Digest]string
	started      map[digest.Digest]time.Time
	ended        map[digest.Digest]time.Time
	lastTransfer time.Time
}

func newSteps(progress Progress) *steps {
	var told sync.Mutex
	one := func(format string, args ...any) {
		told.Lock()
		defer told.Unlock()
		progress(format, args...)
	}
	return &steps{progress: one, since: time.Now(), names: map[digest.Digest]string{}, started: map[digest.Digest]time.Time{}, ended: map[digest.Digest]time.Time{}}
}

func (s *steps) follow(status *client.SolveStatus) {
	for _, v := range status.Vertexes {
		s.names[v.Digest] = v.Name
		var run time.Time
		if v.Started != nil {
			run = *v.Started
		}
		ended, told := s.ended[v.Digest]
		switch {
		case v.Completed != nil && (!told || !ended.Equal(run)):
			s.ended[v.Digest] = run
			switch {
			case v.Error != "":
				s.progress("%s: failed", v.Name)
			case v.Cached:
				s.progress("%s: cached", v.Name)
			case v.Started != nil && v.Completed.Before(s.since):
				s.progress("%s: reused from an earlier build, which ran it in %s", v.Name, v.Completed.Sub(*v.Started).Round(time.Second))
			case v.Started != nil:
				s.progress("%s: done in %s", v.Name, v.Completed.Sub(*v.Started).Round(time.Second))
			default:
				s.progress("%s: done", v.Name)
			}
		case v.Started != nil && v.Completed == nil && !s.started[v.Digest].Equal(run):
			s.started[v.Digest] = run
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
