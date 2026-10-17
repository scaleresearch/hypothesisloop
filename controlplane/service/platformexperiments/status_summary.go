package platformexperiments

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// summarize sets on why, the status_reason of pe whose roster is signups, the sentence a person
// reads at now: what is happening, what it affects and who acts. It is the one producer of that
// sentence, and a reason it has none for is an error.
func summarize(why *domain.StatusReason, pe *domain.PlatformExperiment, signups []domain.AgentSignup, now time.Time) error {
	in := func(at time.Time) string { return readableDuration(max(0, at.Sub(now))) }
	// next is what whoever coordinates does: the hosted coordinator, or else a person of the org.
	next := func(move string) string {
		if pe.CoordinatorHosted {
			return "The coordinator will " + move + "."
		}
		return strings.ToUpper(move[:1]) + move[1:] + "."
	}
	sentence := map[string]func() string{
		"repository_creating": func() string { return "Creating the experiment's repository. Nothing starts until it exists." },
		"planning": func() string {
			return "The planner is working out how to measure your goal. It has up to " + in(pe.PlanningEndsAt) + " more."
		},
		"revising": func() string {
			return "The planner is updating the plan with your change. It has up to " + in(pe.PlanningEndsAt) + " more."
		},
		"accepting": func() string {
			if pe.AcceptsAt == nil {
				return "The plan is ready and starts as proposed in a moment. Accept, edit or reject it before then."
			}
			return "The plan is ready and starts as proposed in " + in(*pe.AcceptsAt) + ". Accept, edit or reject it before then."
		},
		"proposed": func() string {
			return "The plan is ready and waits for your accept. It expires in " + in(pe.ProposalExpiresAt) + "."
		},
		"declined": func() string {
			return "The planner couldn't propose an experiment and says what it needs. Revise the goal or close it; it closes by itself in " + in(pe.ProposalExpiresAt) + "."
		},

		"baseline_measuring": func() string { return measuringBaseline(signups) + " The run starts by itself when it finishes." },
		"baseline_declaring": func() string { return "The baseline is measured and being declared. The run starts next." },
		"baseline_required": func() string {
			return "No usable baseline is measured yet, and the run can't start without one. " + next("add a baseline agent to measure it")
		},
		"baseline_undeclared": func() string {
			return "Several baseline jobs measured the goal's metric, so the platform won't pick one. " + next("declare the one to rank the run against")
		},
		"no_baseline": func() string {
			return "The declared baseline isn't a job of this experiment's baseline agents. " + next("declare a job a baseline agent ran")
		},
		"baseline_not_completed": func() string {
			return "The baseline job didn't finish. " + next("run it again or declare another job")
		},
		"baseline_not_on_branch": func() string {
			return "The baseline job ran on another branch than the baseline's. " + next("declare a job of the baseline branch")
		},
		"baseline_without_finding": func() string {
			return "The baseline job has no summary of what it measured. " + next("have its summary filed")
		},
		"baseline_missing_metric": func() string {
			return "The baseline job didn't report every metric of the experiment. " + next("fix the measurement and run it again")
		},
		"baseline_environment_differs": func() string {
			return "The baseline job ran in another environment than the researchers will. " + next("run it again from the branch's latest commit")
		},
		"baseline_not_in_environment": func() string {
			return "The baseline job didn't run in the environment its commit declares. " + next("run it again")
		},
		"baseline_weaker_than_own_job": func() string {
			return "Another job of the baseline agent measured better than the declared one, and the baseline must be the strongest. " + next("declare that configuration's job")
		},
		"baseline_breaks_constraint": func() string {
			return "The baseline fails a metric's bound, which would disqualify every researcher. Change that bound to continue."
		},
		"budget_below_baseline": func() string {
			return "The budget is too small for each researcher to run one trial at the baseline's measured cost. Raise the budget or allow fewer researchers."
		},
		"starts_at_ahead": func() string { return "The baseline is ready. The run starts by itself in " + in(pe.StartsAt) + "." },
		"no_researchers": func() string {
			return "The baseline is ready, but there is no researcher to run. Add a researcher and the run starts."
		},
		"budget_spent": func() string {
			return "Measuring the baseline used the whole budget. Raise the budget to start the run."
		},
		"starting": func() string { return "Everything is ready. The run starts within a minute." },

		"agents_starting": func() string {
			return "The researchers' machines are starting. If none is running in " + in(pe.AgentsDueAt) + ", the platform closes the experiment."
		},
		"running": func() string { return "The researchers are at work. The run ends in " + in(pe.EndsAt) + "." },
		"researchers_done": func() string {
			return "No researcher is left to submit jobs. " + next("add a researcher, or write the summary and close the experiment") + " It closes by itself in " + in(pe.EndsAt) + "."
		},

		string(domain.CloseEndTimeReached): func() string { return "Closed: it reached its end time." },
		string(domain.CloseByRequest): func() string {
			who := "its owner"
			if pe.ClosedBy != nil && pe.ClosedBy.Name != "" {
				who = pe.ClosedBy.Name
			}
			if said := strings.Join(strings.Fields(pe.CloseMessage), " "); said != "" {
				return fmt.Sprintf("Closed by %s: %q", who, said)
			}
			return "Closed by " + who + "."
		},
		string(domain.ClosePlanningLimitReached): func() string { return "Closed: the planner proposed nothing in time." },
		string(domain.CloseProposalExpired):      func() string { return "Closed: nobody accepted or revised the plan in time." },
		string(domain.CloseBaselineLimitReached): func() string {
			if pe.BaselineJobID != "" {
				return "Closed: the baseline was ready, but no researcher or budget started the run in time."
			}
			return "Closed: no baseline was measured in time."
		},
		string(domain.ClosePlannerStopped): func() string {
			return "Closed: the planner's machine failed before it answered."
		},
		string(domain.CloseAgentsNotStarted): func() string {
			return "Closed: no researcher's machine started in time."
		},
		string(domain.CloseRepositoryNotCreated): func() string {
			return "Closed: its repository couldn't be created: " + pe.RepositoryError
		},
		"closed": func() string { return "Closed before the platform recorded why." },
	}
	say, known := sentence[why.Reason]
	if !known {
		return fmt.Errorf("status reason %q has no summary", why.Reason)
	}
	why.Summary = say()
	return nil
}

// measuringBaseline names the baseline agents of signups at work on the baseline.
func measuringBaseline(signups []domain.AgentSignup) string {
	var names []string
	for _, signup := range signups {
		if signup.Role == domain.SignupRoleBaseline && signup.FinishedAt == nil && (signup.Instance == nil || !signup.Instance.Stopped) {
			names = append(names, signup.Name)
		}
	}
	switch len(names) {
	case 0:
		return "The baseline agent is measuring the baseline."
	case 1:
		return names[0] + " is measuring the baseline."
	}
	return strings.Join(names, " and ") + " are measuring the baseline."
}

// readableDuration is d as a person says it: "less than a minute", "5 minutes", "4 hours", "3 days".
func readableDuration(d time.Duration) string {
	count := func(n float64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", int(n), unit)
	}
	if d < time.Minute {
		return "less than a minute"
	}
	if minutes := math.Round(d.Minutes()); minutes < 60 {
		return count(minutes, "minute")
	}
	if hours := math.Round(d.Hours()); hours < 48 {
		return count(hours, "hour")
	}
	return count(math.Round(d.Hours()/24), "day")
}
