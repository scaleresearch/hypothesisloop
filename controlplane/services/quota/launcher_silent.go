package quota

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// reasonLauncherUnreachable is the status_reason of a platform experiment, and of an agent of its
// roster, whose machine waits for a launcher that has not asked for its machines lately.
const reasonLauncherUnreachable = "launcher_unreachable"

// launcherSilentAfter is how long a launcher may go without asking for its machines before what
// it is to run says it waits for it: several of its passes, which are seconds apart.
const launcherSilentAfter = time.Minute

// wantsMachine says pe wants a machine now for signup, a hosted agent of it: a draft's planner
// while it plans in its repository, an open one's baseline agents until they finish, and a
// running one's agents whose run goes on.
func wantsMachine(pe *domain.PlatformExperiment, signup domain.AgentSignup) bool {
	if !signup.Hosted || signup.Ended != "" || signup.Instance != nil && signup.Instance.Stopped {
		return false
	}
	switch pe.Status {
	case domain.PlatformExpDraft:
		return signup.Role == domain.SignupRolePlanner && pe.Repository != "" && !pe.Answered()
	case domain.PlatformExpOpen:
		return signup.Role == domain.SignupRoleBaseline && signup.FinishedAt == nil
	case domain.PlatformExpRunning:
		return signup.Role != domain.SignupRolePlanner
	}
	return false
}

// silentLaunchers is, of the launchers named, the sentence for each that has not asked for its
// machines for launcherSilentAfter, by name.
func (s *PlatformExperimentsService) silentLaunchers(ctx context.Context, names []string, now time.Time) (map[string]string, error) {
	silent := map[string]string{}
	if len(names) == 0 {
		return silent, nil
	}
	answered, err := s.store.LauncherAnsweredAt(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.silentLaunchers: %w", err)
	}
	for _, name := range names {
		at := answered[name]
		switch {
		case at == nil:
			silent[name] = fmt.Sprintf("launcher %s has not asked for its machines since it was registered", name)
		case now.Sub(*at) > launcherSilentAfter:
			silent[name] = fmt.Sprintf("launcher %s last asked for its machines %s ago", name, now.Sub(*at).Round(time.Second))
		}
	}
	return silent, nil
}

// launcherUnreachable is the status_reason of pe, whose roster is signups, while a launcher that is
// to run a machine it wants is silent, nil while every such launcher asks.
func (s *PlatformExperimentsService) launcherUnreachable(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*domain.StatusReason, error) {
	var names []string
	for _, signup := range signups {
		if wantsMachine(pe, signup) {
			names = append(names, signup.Launcher)
		}
	}
	if pe.Coordinator != nil && !pe.Coordinator.Instance.Stopped && (pe.Status == domain.PlatformExpOpen || pe.Status == domain.PlatformExpRunning) {
		names = append(names, pe.Coordinator.Launcher)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	silent, err := s.silentLaunchers(ctx, names, time.Now())
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if sentence, ok := silent[name]; ok {
			return &domain.StatusReason{Reason: reasonLauncherUnreachable, Message: launcherUnreachableMessage(sentence)}, nil
		}
	}
	return nil, nil
}

func launcherUnreachableMessage(sentence string) string {
	return sentence + ", so no machine it is to run starts or is watched until it asks again. Its deadlines still count. The platform's operator starts the launcher (agent-launcher), and nobody else acts"
}
