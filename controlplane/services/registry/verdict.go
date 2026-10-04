package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Verdict is what an agent says of a hypothesis. confirmed and refuted are claims that a
// measurement differs from a reference, so each needs what such a claim rests on.
type Verdict struct {
	Status domain.HypothesisStatus
	// Against is the control: the job the hypothesis's own jobs are compared with.
	Against string
	// Despite is the agent's reason why the verdict stands without that evidence.
	Despite string
}

// minJobsForAVerdict is how many completed jobs of its own a hypothesis needs, besides the
// control, before it is confirmed or refuted: one measurement and one repeat of it. A single
// run does not tell an effect from the spread between two runs of the same thing.
const minJobsForAVerdict = 2

// minDespiteLength keeps a reason from being a word.
const minDespiteLength = 20

// UnsupportedVerdictError refuses a confirmed or refuted verdict the record does not carry. Its
// text is for the agent that asked: what is there, what is missing and what to do next.
type UnsupportedVerdictError struct{ Detail string }

func (e *UnsupportedVerdictError) Error() string { return e.Detail }

// supported checks a confirmed or refuted verdict against the jobs on record. The rule knows
// nothing of the task: it asks for a repeated measurement and a named control, both finished,
// both of this platform experiment. What it cannot see (a deterministic job, whose repeat gives
// the same numbers, a failure that needs no repeat, a control measured elsewhere) the agent
// states in Despite, which is kept beside the verdict.
func (s *Service) supported(ctx context.Context, h *domain.Hypothesis, v Verdict) error {
	if v.Status != domain.HypothesisConfirmed && v.Status != domain.HypothesisRefuted {
		return nil
	}
	if reason := strings.TrimSpace(v.Despite); reason != "" {
		if len(reason) < minDespiteLength {
			return &UnsupportedVerdictError{Detail: fmt.Sprintf("the reason given with despite is %d characters, and it takes at least %d characters: say in a sentence why this verdict holds without a repeated run and a control (hl hypothesis status %s %s --despite \"<why>\")", len(reason), minDespiteLength, h.ID, v.Status)}
		}
		return nil
	}
	tested, err := s.store.ListExperimentsTesting(ctx, []string{h.ID})
	if err != nil {
		return err
	}
	var own []string
	for _, job := range tested {
		if job.Status == domain.StatusCompleted && job.ID != v.Against {
			own = append(own, job.ID)
		}
	}
	otherwise := fmt.Sprintf("Until then the verdict is inconclusive. A repeat is needed only where results vary between runs: "+
		"for a deterministic job (the same code and data give the same numbers), a failure that needs no repeat, or a control "+
		"measured outside this experiment, give the verdict with despite, a sentence of at least %d characters saying why it "+
		"holds without them: hl hypothesis status %s %s --despite \"<why>\"", minDespiteLength, h.ID, v.Status)
	if v.Against == "" {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("%s says the hypothesis's jobs differ from a control, and no control is named: give the completed job you compare with as against (a baseline run, or the same job without your change; run one if there is none). %s", v.Status, otherwise)}
	}
	control, err := s.store.GetExperiment(ctx, v.Against)
	if err != nil {
		return err
	}
	if control == nil || control.PlatformExperimentID != h.PlatformExperimentID {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("the control %s is no job of this platform experiment. %s", v.Against, otherwise)}
	}
	if control.Check {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("the control %s is a check (hl env run), which measures nothing: name a completed job of hl job submit. %s", v.Against, otherwise)}
	}
	if control.Status != domain.StatusCompleted {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("the control %s is %s, and a control is a job that completed. %s", v.Against, control.Status, otherwise)}
	}
	if len(own) < minJobsForAVerdict {
		have := "none"
		if len(own) > 0 {
			have = strings.Join(own, ", ")
		}
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("%s needs %d completed jobs on the hypothesis besides the control %s, and it has %d (%s): where results vary between runs, one run does not tell an effect from that spread, so run it again. %s", v.Status, minJobsForAVerdict, v.Against, len(own), have, otherwise)}
	}
	return nil
}

// basis is the note kept on the hypothesis with a confirmed or refuted verdict, so that whoever
// repeats the claim can read what it rests on.
func basis(v Verdict) string {
	if reason := strings.TrimSpace(v.Despite); reason != "" {
		return fmt.Sprintf("%sgiven without a repeated run and a control: %s", basisPrefix(v.Status), reason)
	}
	return fmt.Sprintf("%sagainst control %s.", basisPrefix(v.Status), v.Against)
}

func basisPrefix(status domain.HypothesisStatus) string {
	return fmt.Sprintf("Verdict %s ", status)
}

// basisPage is how many comments basisNoted reads at a time.
const basisPage = 200

// basisNoted reports whether the basis of the verdict h holds is among its comments: a basis of
// that status written since the status was set.
func (s *Service) basisNoted(ctx context.Context, h *domain.Hypothesis) (bool, error) {
	for offset := 0; ; offset += basisPage {
		comments, err := s.store.ListCommentsByHypothesis(ctx, h.ID, basisPage, offset)
		if err != nil {
			return false, err
		}
		for _, c := range comments {
			if strings.HasPrefix(c.Text, basisPrefix(h.Status)) && (h.StatusChangedAt == nil || !c.CreatedAt.Before(*h.StatusChangedAt)) {
				return true, nil
			}
		}
		if len(comments) < basisPage {
			return false, nil
		}
	}
}
