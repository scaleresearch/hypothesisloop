// Package ranktail is the log tail of a job that spans several nodes, as every runtime shows it:
// one section per rank, sharing one budget of lines.
package ranktail

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// SectionLines is the least a rank's section is given, its header included. A gang with more
// ranks than the budget holds at that size shows only the ranks that explain it most.
const SectionLines = 10

// Section is one rank of a gang as its log tail shows it. Where names what ran it, such as
// "pod P" or "instance I", and Read returns the last n lines it wrote.
type Section struct {
	Rank     int
	Group    string
	Where    string
	Exit     int
	FailedAt time.Time
	Read     func(n int) ([]string, error)
}

func (s Section) Header() string {
	where := s.Where
	if s.Group != "" {
		where = "group " + s.Group + ", " + where
	}
	if s.Exit > 0 {
		return fmt.Sprintf("--- rank %d failed, exit %d (%s) ---", s.Rank, s.Exit, where)
	}
	return fmt.Sprintf("--- rank %d (%s) ---", s.Rank, where)
}

// Tail is a gang's tail: the sections Pick keeps of maxLines, read in its order.
func Tail(sections []Section, maxLines int) ([]string, error) {
	shown, omitted := Pick(sections, max(1, maxLines/SectionLines))
	return assemble(shown, omitted, maxLines)
}

// Pick keeps at most shown sections of a gang and orders them for its tail, with how many it left
// out. Every consumer of a tail reads its end (the failure cause keeps the last lines, hl job logs
// prints the last ones), so the section that explains the job comes last: the other ranks by
// rank, then rank 0, then the failed ranks, the first to fail at the very end, since the rest
// usually die of it. When not every rank fits, the failed ranks are kept first and rank 0 next.
func Pick(sections []Section, shown int) ([]Section, int) {
	var failed, rest []Section
	for _, s := range sections {
		if s.Exit > 0 {
			failed = append(failed, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.Slice(failed, func(i, j int) bool {
		if !failed[i].FailedAt.Equal(failed[j].FailedAt) {
			return failed[i].FailedAt.Before(failed[j].FailedAt)
		}
		return failed[i].Rank < failed[j].Rank
	})
	sort.Slice(rest, func(i, j int) bool { return rest[i].Rank < rest[j].Rank })

	omitted := max(0, len(failed)+len(rest)-shown)
	failed = failed[:min(len(failed), shown)]
	rest = rest[:min(len(rest), shown-len(failed))]
	slices.Reverse(failed)
	if len(rest) > 0 && rest[0].Rank == 0 {
		rest = append(rest[1:], rest[0])
	}
	return append(rest, failed...), omitted
}

// assemble reads each section's tail in order, each given an equal share of what is left of
// maxLines, so a short section leaves its unused lines to the ones after it.
func assemble(sections []Section, omitted, maxLines int) ([]string, error) {
	var lines []string
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("--- %d more ranks not shown ---", omitted))
	}
	for i, s := range sections {
		share := (maxLines - len(lines)) / (len(sections) - i)
		body, err := s.Read(max(1, share-1))
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			continue
		}
		lines = append(lines, s.Header())
		lines = append(lines, body...)
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}
