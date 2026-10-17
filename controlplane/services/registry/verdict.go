package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
// run does not tell an effect from the spread between two runs of the same thing, unless every
// ranking metric is declared deterministic, where a repeat gives the same numbers.
const minJobsForAVerdict = 2

// minDespiteLength keeps a reason from being a word.
const minDespiteLength = 20

// UnsupportedVerdictError refuses a confirmed or refuted verdict the record does not carry. Its
// text is for the agent that asked: what is there, what is missing and what to do next.
type UnsupportedVerdictError struct{ Detail string }

func (e *UnsupportedVerdictError) Error() string { return e.Detail }

// supported checks a verdict against the jobs on record and returns the
// basis to note with it. Every verdict but open needs a job of the hypothesis's own that completed
// or failed, which a reason never replaces. The rule knows nothing of the task: it asks for a repeated measurement
// and a named control, both finished, both of this platform experiment, and no repeat where every
// ranking metric is declared deterministic. What it cannot see (a failure that needs no repeat, a
// control measured elsewhere) the agent states in Despite, which is kept beside the verdict. A
// control that holds is the basis even when a reason comes with it. Where every ranking metric is
// deterministic, a hypothesis with a completed job of its own has all the record needs but the
// control, so a reason does not stand in for naming it.
func (s *Service) supported(ctx context.Context, h *domain.Hypothesis, v Verdict) (string, error) {
	if v.Status == domain.HypothesisOpen {
		return "", nil
	}
	tested, err := s.store.ListExperimentsTesting(ctx, []string{h.ID})
	if err != nil {
		return "", err
	}
	finished := false
	for _, job := range tested {
		finished = finished || job.Status == domain.StatusCompleted || job.Status == domain.StatusFailed
	}
	if !finished {
		return "", &UnsupportedVerdictError{Detail: fmt.Sprintf("%s needs a job of this hypothesis that completed or failed: run one (hl job submit FILE --hypothesis %s), its metrics decide the verdict", v.Status, h.ID)}
	}
	if v.Status == domain.HypothesisInconclusive {
		return "", nil
	}
	reason := strings.TrimSpace(v.Despite)
	if reason != "" && len(reason) < minDespiteLength {
		return "", &UnsupportedVerdictError{Detail: fmt.Sprintf("the reason given with despite is %d characters, and it takes at least %d characters: say in a sentence why this verdict holds without a repeated run and a control (hl hypothesis status %s %s --despite \"<why>\")", len(reason), minDespiteLength, h.ID, v.Status)}
	}
	var own []string
	for _, job := range tested {
		if job.Status == domain.StatusCompleted && job.ID != v.Against {
			own = append(own, job.ID)
		}
	}
	metrics, found, err := s.store.GetPlatformExperimentMetrics(ctx, h.PlatformExperimentID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, h.PlatformExperimentID)
	}
	deterministic := domain.AllRankingDeterministic(metrics)
	if v.Against != "" {
		err := s.controlled(ctx, h, v, own, deterministic)
		var unsupported *UnsupportedVerdictError
		switch {
		case err == nil:
			return fmt.Sprintf("%sagainst control %s.", basisPrefix(v.Status), v.Against), nil
		case reason == "" || !errors.As(err, &unsupported):
			return "", err
		}
	} else if reason == "" {
		return "", &UnsupportedVerdictError{Detail: fmt.Sprintf("%s says the hypothesis's jobs differ from a control, and no control is named: give the completed job you compare with as against (a baseline run, or the same job without your change; run one if there is none). %s", v.Status, otherwise(h, v, deterministic))}
	}
	if deterministic && len(own) > 0 {
		return "", &UnsupportedVerdictError{Detail: fmt.Sprintf("every ranking metric of this platform experiment is declared deterministic, so %s with a completed control settles the verdict, with no repeat and no reason in its place: name the control (the baseline's job, baseline_job_id in hl platform-experiments get, or the same job without your change) with hl hypothesis status %s %s --against CONTROL_JOB", strings.Join(own, ", "), h.ID, v.Status)}
	}
	return fmt.Sprintf("%sgiven without a repeated run and a control: %s", basisPrefix(v.Status), reason), nil
}

// controlled checks the control v names and the completed jobs own the hypothesis has besides it.
func (s *Service) controlled(ctx context.Context, h *domain.Hypothesis, v Verdict, own []string, deterministic bool) error {
	control, err := s.store.GetExperiment(ctx, v.Against)
	if err != nil {
		return err
	}
	if control == nil || control.PlatformExperimentID != h.PlatformExperimentID {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("the control %s is no job of this platform experiment. %s", v.Against, otherwise(h, v, deterministic))}
	}
	if control.Status != domain.StatusCompleted {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("the control %s is %s, and a control is a job that completed. %s", v.Against, control.Status, otherwise(h, v, deterministic))}
	}
	needed := minJobsForAVerdict
	if deterministic {
		needed = 1
	}
	if len(own) >= needed {
		return nil
	}
	if needed == 1 {
		return &UnsupportedVerdictError{Detail: fmt.Sprintf("%s needs a completed job on the hypothesis besides the control %s, and it has none: run it (hl job submit FILE --hypothesis %s) and set the verdict once it completed. %s", v.Status, v.Against, h.ID, otherwise(h, v, deterministic))}
	}
	have := "none"
	if len(own) > 0 {
		have = strings.Join(own, ", ")
	}
	return &UnsupportedVerdictError{Detail: fmt.Sprintf("%s needs %d completed jobs on the hypothesis besides the control %s, and it has %d (%s): where results vary between runs, one run does not tell an effect from that spread, so run it again (hl job submit FILE --hypothesis %s, the same submit as before with --repeat 2 for a second run of it) and set the verdict once it completed. %s", v.Status, needed, v.Against, len(own), have, h.ID, otherwise(h, v, deterministic))}
}

// otherwise ends a refusal with what the verdict is until the record holds it, and when a reason
// may stand in for what the record lacks.
func otherwise(h *domain.Hypothesis, v Verdict, deterministic bool) string {
	without := "A repeat is needed only where results vary between runs: for a deterministic job on a metric not declared so (the same code and data give the same numbers), a failure that needs no repeat, or a control measured outside this experiment"
	if deterministic {
		without = "Every ranking metric here is declared deterministic, so one completed job and a control settle it with no repeat. For a verdict the hypothesis's jobs cannot show on their own (a failure that needs no repeat)"
	}
	return fmt.Sprintf("Until then the verdict is inconclusive. %s, give the verdict with despite, a sentence of at least %d characters saying why it holds without them: hl hypothesis status %s %s --despite \"<why>\"", without, minDespiteLength, h.ID, v.Status)
}

// basisPrefix starts the note kept on the hypothesis with a confirmed or refuted verdict, so that
// whoever repeats the claim can read what it rests on.
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
