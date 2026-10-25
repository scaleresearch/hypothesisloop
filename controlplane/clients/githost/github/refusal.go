package github

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// The reasons a client tells a refusal about GitHub apart by.
const (
	ReasonInstallationNotConnected       = "installation_not_connected"
	ReasonInstallationGone               = "installation_gone"
	ReasonInstallationSuspended          = "installation_suspended"
	ReasonInvalidRepository              = "invalid_repository"
	ReasonRepositoryNotReachable         = "repository_not_reachable"
	ReasonRepositoryNotCreated           = "repository_not_created"
	ReasonRepositoryExists               = "repository_exists"
	ReasonRepositoryCreationNotPermitted = "repository_creation_not_permitted"
	ReasonRepositoryNotAdded             = "repository_not_added"
	ReasonRepositoryAccountRequired      = "repository_account_required"
	ReasonUnreadableFile                 = "unreadable_file"
	ReasonUnavailable                    = "unavailable"
	ReasonAuthorizationInvalid           = "github_authorization_invalid"
)

// ErrNotConnected reports a repository whose owner is no GitHub account the org connected.
var ErrNotConnected = errors.New("the org has connected no installation on that GitHub account")

// ErrNoInstallation reports an org that connected no GitHub account at all.
var ErrNoInstallation = errors.New("the org has connected no GitHub account")

// InstallationGoneError is ErrInstallationGone for the installation an org connected on Account,
// and ErrInstallationSuspended with Suspended.
type InstallationGoneError struct {
	Account      string
	Installation int64
	Suspended    bool
}

func (e *InstallationGoneError) Error() string {
	if e.Suspended {
		return fmt.Sprintf("github: installation %d on %s was suspended", e.Installation, e.Account)
	}
	return fmt.Sprintf("github: installation %d on %s was removed", e.Installation, e.Account)
}

func (e *InstallationGoneError) Unwrap() error {
	if e.Suspended {
		return ErrInstallationSuspended
	}
	return ErrInstallationGone
}

// Refusal turns what GitHub refused into what the caller is told. gone is the status of a
// repository the org's installations do not reach: 422 where a request names it, 409 where a
// platform experiment already does. GitHub failing a call for an installation is 502
// git_host_failed, naming the installation and what GitHub answered. Anything else is the
// platform's failure or GitHub's: it is logged and answered without its text, but for a create
// GitHub refused, whose answer is GitHub's own message.
func Refusal(logger *zap.Logger, op string, err error, gone int) error {
	var status interface{ GetStatus() int }
	var notCovered *NotCoveredError
	var notPermitted *CreationNotPermittedError
	var failed *CreationFailedError
	var notAsAsked *NotAsAskedError
	var githubFailed *FailedError
	var missing *PermissionMissingError
	var installationGone *InstallationGoneError
	switch {
	case errors.As(err, &status):
		return err
	case errors.As(err, &notCovered):
		logger.Error("github created a repository and did not add it to the installation", zap.String("operation", op), zap.Error(err))
		return refusal.Refuse(http.StatusBadGateway, ReasonRepositoryNotAdded, "GitHub created the repository "+notCovered.Repository+" and did not let the platform add it to the GitHub App's installation, which covers selected repositories only, so nothing else was created. Add it to the installation on GitHub (the account's Settings, Applications, Configure), then name it in repository on the next try")
	case errors.As(err, &notAsAsked):
		logger.Error("github created a repository that is not the private one asked for", zap.String("operation", op), zap.Error(err))
		return refusal.Refuse(http.StatusBadGateway, ReasonRepositoryNotCreated, "GitHub created the repository "+notAsAsked.Repository+", which is not the private repository asked for on the GitHub account the org connected, so nothing else was created. It is left on GitHub: its owner deletes it there, or makes it private and adds it to the installation, and the request names it in repository on the next try")
	case errors.Is(err, ErrInvalidRepository):
		return refusal.Invalid(ReasonInvalidRepository, "repository must be owner/name, as in the repository's address on GitHub")
	case errors.Is(err, ErrNoInstallation):
		return refusal.Invalid(githost.ReasonNotConnected, "a platform experiment keeps its code in a GitHub repository, and the org has connected no GitHub account: an owner of the org installs the GitHub App on a GitHub account in Settings, GitHub, and the experiment then names a repository there or the platform creates one")
	case errors.As(err, &notPermitted):
		accepted := ""
		if notPermitted.Accepted != "" {
			accepted = " GitHub takes for it one of the permissions " + strconv.Quote(notPermitted.Accepted) + "."
		}
		return refusal.Invalid(ReasonRepositoryCreationNotPermitted, "GitHub does not let the platform's GitHub App create a repository on the GitHub account the org connected."+accepted+" The owner of the account accepts the App's permissions on GitHub and lets the App create repositories there, or creates a private repository on it, adds it to the installation when that covers selected repositories only, and the request names it in repository")
	case errors.Is(err, ErrRepositoryExists):
		return refusal.Refuse(http.StatusConflict, ReasonRepositoryExists, "the GitHub account the org connected already has a repository of the name this platform experiment's would take: create it again, or name a repository")
	case errors.As(err, &failed):
		logger.Error("github refused to create a repository", zap.String("operation", op), zap.Error(err))
		return refusal.Refuse(http.StatusBadGateway, ReasonRepositoryNotCreated, "GitHub did not create the repository, so nothing else was created. It answered "+strconv.Itoa(failed.Status)+": "+strconv.Quote(failed.Message)+". Name a repository the installation covers, or try again")
	case errors.Is(err, ErrNotConnected):
		return refusal.Refuse(gone, ReasonInstallationNotConnected, "the org has connected no GitHub installation on that GitHub account, the one that owns the repository or the one a new repository was asked on: an owner of the org installs the GitHub App on it in Settings, GitHub")
	case errors.Is(err, ErrRepositoryNotInstalled):
		return refusal.Refuse(gone, ReasonRepositoryNotReachable, "the GitHub installation the org connected does not cover that repository: its owner adds it to the installation on GitHub")
	case errors.As(err, &installationGone) && installationGone.Suspended:
		return refusal.Refuse(gone, ReasonInstallationSuspended, "the GitHub App is suspended on "+installationGone.Account+" (installation "+strconv.FormatInt(installationGone.Installation, 10)+"): its owner unsuspends it on GitHub, in the account's Settings, Applications")
	case errors.As(err, &installationGone):
		return refusal.Refuse(gone, ReasonInstallationGone, "the GitHub App is no longer installed on "+installationGone.Account+" (installation "+strconv.FormatInt(installationGone.Installation, 10)+"): its owner installs the App again from Settings, GitHub")
	case errors.Is(err, ErrInstallationSuspended):
		return refusal.Refuse(gone, ReasonInstallationSuspended, "the GitHub App installation is suspended: its owner unsuspends it on GitHub, in the account's Settings, Applications")
	case errors.Is(err, ErrInstallationGone):
		return refusal.Refuse(gone, ReasonInstallationGone, "the GitHub App installation was removed on GitHub: its owner installs the App again from Settings, GitHub")
	case errors.As(err, &missing):
		return refusal.Refuse(gone, githost.ReasonPermissionMissing, "permissions not granted: "+strings.Join(missing.Missing, ", ")+". The GitHub App installation has not granted that, which the platform needs: its owner accepts the App's permissions on GitHub (the account's Settings, Applications, Configure)")
	case errors.Is(err, ErrPermissionNotGranted):
		return refusal.Refuse(gone, githost.ReasonPermissionMissing, "the GitHub App installation has not granted the access the App asks for: its owner accepts the App's permissions on GitHub (the account's Settings, Applications, Configure)")
	case errors.Is(err, ErrAuthorizationInvalid):
		return refusal.Invalid(ReasonAuthorizationInvalid, "GitHub did not take the authorization this return from GitHub carries: it was used already or ran out. Press Connect GitHub again")
	case errors.Is(err, ErrRateLimited):
		logger.Warn("github is rate limiting the platform", zap.String("operation", op))
		return refusal.Refuse(http.StatusServiceUnavailable, ReasonUnavailable, "GitHub is rate limiting the platform: try again shortly")
	case errors.Is(err, ErrAppCredentials):
		logger.Error("github refused the App's credentials", zap.String("operation", op), zap.Error(err))
		return refusal.Refuse(http.StatusBadGateway, ReasonUnavailable, "the platform could not authenticate to GitHub")
	case errors.As(err, &githubFailed):
		logger.Error("github failed a call for an installation", zap.String("operation", op), zap.Int64("installationID", githubFailed.Installation), zap.Error(err))
		answered := "did not answer"
		if githubFailed.Status != 0 {
			answered = "answered " + strconv.Itoa(githubFailed.Status) + " " + strconv.Quote(githubFailed.Message)
		}
		return refusal.Refuse(http.StatusBadGateway, githost.ReasonFailed, "GitHub failed the platform's call for GitHub App installation "+strconv.FormatInt(githubFailed.Installation, 10)+": it "+answered+". Try again later, and if GitHub keeps failing, the owner of the GitHub App reports it to GitHub Support with the installation id")
	default:
		return auth.Internal(logger, "github request failed", err, zap.String("operation", op))
	}
}
