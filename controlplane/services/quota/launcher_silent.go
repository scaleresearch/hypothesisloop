package quota

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// reasonLauncherUnreachable is the status_reason of a platform experiment, and of an agent of its
// roster, whose machine waits for a launcher that has not checked in lately, and why hosting is
// unavailable while the configured launcher has not.
const reasonLauncherUnreachable = "launcher_unreachable"

// reasonLauncherNotRegistered is why hosting is unavailable while no launcher is registered under
// the configured name.
const reasonLauncherNotRegistered = "launcher_not_registered"

// launcherSilentAfter is how long a launcher may go without checking in before what it is to run
// says it waits for it: several of its passes, which are seconds apart.
const launcherSilentAfter = time.Minute

// wantsMachine says pe, which has its repository, wants a machine now for signup, a hosted agent
// of it: a draft's planner while it plans, an open one's baseline agents until they finish, and a
// running one's agents whose run goes on.
func wantsMachine(pe *domain.PlatformExperiment, signup domain.AgentSignup) bool {
	if pe.AwaitsRepository() || !signup.Hosted || signup.Ended != "" || signup.Instance != nil && signup.Instance.Stopped {
		return false
	}
	switch pe.Status {
	case domain.PlatformExpDraft:
		return signup.Role == domain.SignupRolePlanner && !pe.Answered()
	case domain.PlatformExpOpen:
		return signup.Role == domain.SignupRoleBaseline && signup.FinishedAt == nil
	case domain.PlatformExpRunning:
		return signup.Role != domain.SignupRolePlanner
	}
	return false
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

// launcherSilence is the status_reason of what waits for the launcher name, which last checked in
// at (nil for never), at now: nil while it checks in. Staff are told what to check.
func launcherSilence(name string, at *time.Time, now time.Time, staff bool) *domain.StatusReason {
	why := &domain.StatusReason{Reason: reasonLauncherUnreachable, Since: at}
	var silence string
	switch {
	case at == nil:
		why.Message = fmt.Sprintf("launcher %s has not checked in since it was registered", name)
		why.Summary = "Agents can't start yet. They start automatically once they can."
		silence = "has never checked in"
	case now.Sub(*at) > launcherSilentAfter:
		ago := readableDuration(now.Sub(*at))
		why.Message = fmt.Sprintf("launcher %s last checked in %s ago", name, ago)
		why.Summary = "Agents are paused and will resume automatically."
		silence = "last checked in " + ago + " ago"
	default:
		return nil
	}
	why.Message += ", so none of its machines starts or is watched until it checks in again. Deadlines keep running, and only the platform's operator brings the launcher (agent-launcher) back"
	if staff {
		why.OperatorDetail = fmt.Sprintf("The agent-launcher registered as %q %s. Start it or check its logs (on a dev stack: make dev-hosted).", name, silence)
	}
	return why
}

// silentLaunchers is, of the launchers named, the status_reason of what waits for each that has
// not checked in for launcherSilentAfter, by name.
func (s *PlatformExperimentsService) silentLaunchers(ctx context.Context, names []string, now time.Time, staff bool) (map[string]*domain.StatusReason, error) {
	silent := map[string]*domain.StatusReason{}
	if len(names) == 0 {
		return silent, nil
	}
	answered, err := s.store.LauncherAnsweredAt(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.silentLaunchers: %w", err)
	}
	for _, name := range names {
		if why := launcherSilence(name, answered[name], now, staff); why != nil {
			silent[name] = why
		}
	}
	return silent, nil
}

// launcherUnreachable is the status_reason of pe, whose roster is signups, while a launcher that is
// to run a machine it wants is silent, nil while every such launcher checks in.
func (s *PlatformExperimentsService) launcherUnreachable(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup, staff bool) (*domain.StatusReason, error) {
	var names []string
	for _, signup := range signups {
		if wantsMachine(pe, signup) {
			names = append(names, signup.Launcher)
		}
	}
	if pe.Coordinator != nil && !pe.Coordinator.Instance.Stopped && !pe.AwaitsRepository() && (pe.Status == domain.PlatformExpOpen || pe.Status == domain.PlatformExpRunning) {
		names = append(names, pe.Coordinator.Launcher)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	silent, err := s.silentLaunchers(ctx, names, time.Now(), staff)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if why, ok := silent[name]; ok {
			return why, nil
		}
	}
	return nil, nil
}

// Hosting is whether the platform can host an agent now: GET /hosting, and what every request
// that hosts one is refused with while it cannot.
type Hosting struct {
	Available   bool                 `json:"available"`
	Unavailable *domain.StatusReason `json:"unavailable,omitempty" doc:"Why it cannot, while available is false: reason launcher_not_registered or launcher_unreachable."`
}

// hosting is whether the platform can host an agent now on launcher, the one the config gives new
// machines to: it is registered and checked in within launcherSilentAfter. Staff are told what
// to check.
func (s *PlatformExperimentsService) hosting(ctx context.Context, launcher string, staff bool) (Hosting, error) {
	answered, err := s.store.LauncherAnsweredAt(ctx, []string{launcher})
	if err != nil {
		return Hosting{}, fmt.Errorf("platform_experiments.hosting: %w", err)
	}
	at, registered := answered[launcher]
	if !registered {
		why := &domain.StatusReason{
			Reason:  reasonLauncherNotRegistered,
			Message: fmt.Sprintf("the platform gives hosted machines to launcher %q (config agents.launcher), which is not registered: an operator registers it with `hl-admin launcher add %s`", launcher, launcher),
			Summary: "Hosted agents aren't available on this platform yet.",
		}
		if staff {
			why.OperatorDetail = fmt.Sprintf("No launcher is registered as %q (config agents.launcher). Register it with hl-admin launcher add %s and start the agent-launcher service (on a dev stack: make dev-hosted).", launcher, launcher)
		}
		return Hosting{Unavailable: why}, nil
	}
	why := launcherSilence(launcher, at, time.Now(), staff)
	if why == nil {
		return Hosting{Available: true}, nil
	}
	why.Message = "nothing can be hosted now: " + why.Message
	if at == nil {
		why.Summary = "Hosted agents aren't available right now. They become available automatically."
	} else {
		why.Summary = "Hosted agents aren't available right now. They become available automatically."
	}
	return Hosting{Unavailable: why}, nil
}

// requireHosting refuses to host anything while the platform cannot (hosting): nothing would
// ever start it.
func (s *PlatformExperimentsService) requireHosting(ctx context.Context, launcher string) error {
	caller, _ := auth.FromContext(ctx)
	state, err := s.hosting(ctx, launcher, caller.Kind == auth.KindStaff)
	if err != nil {
		return err
	}
	if why := state.Unavailable; why != nil {
		refusal := api.Refuse(http.StatusServiceUnavailable, why.Reason, why.Reason+": "+why.Message)
		refusal.Summary, refusal.OperatorDetail, refusal.Since = why.Summary, why.OperatorDetail, why.Since
		return refusal
	}
	return nil
}
