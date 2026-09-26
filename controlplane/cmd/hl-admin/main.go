// hl-admin is the operator's hand on tenancy for a deployment that has the signing secret and
// the database: it mints derived tokens, registers users, API keys and clusters, and decides who
// is staff and which clusters are shared. Local development and the e2e suite set themselves up
// with it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

const usage = `usage: hl-admin <command>

  token SCOPE                      print the token for a scope: staff, runtime:<cluster>,
                                   agent:<platform experiment>/<agent>, coordinator:<platform experiment>,
                                   job:<experiment>, launcher:<name>
  user add SUBJECT NAME            create a user named NAME with a home org named NAME and its default project.
                                   A person who signs in gets theirs named Default org
  user staff SUBJECT on|off        make a user one of the platform's operators, or stop
  api-key add SUBJECT ORG [PROJECT]  create an API key of ORG, bound to PROJECT when given
  cluster add NAME ORG             register a cluster for ORG and print its runtime token
  cluster share NAME on|off        place every org's jobs on a cluster, or only its own org's
  platform-experiment share ID     let every account read a platform experiment and what is in it
  platform-experiment unshare ID   take that back: its own org alone reads it again
  launcher add NAME                register an agent-launcher and print its token
  launcher remove NAME             end a launcher's token. Empty it first: what it still holds
                                   is then nobody's to take down
  github connect ORG INSTALLATION_ID
                                   connect an installation of the platform's GitHub App to an org, as
                                   an operator who vouches for it. GitHub is asked, as the App, which
                                   account the installation is on. The API's own connect also asks a
                                   person to prove they reach the installation, and this does not

token reads HYPOTHESISLOOP_CONFIG. The others also read DATABASE_URL, and github connect needs the
github section of the config.`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hl-admin: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 2 && args[0] == "token":
		token, err := auth.Mint(cfg.Auth.SigningSecret, args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, token)
		return nil
	case len(args) == 4 && args[0] == "user" && args[1] == "add":
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			return addUser(ctx, db.NewAccountsStore(pool), args[2], args[3], stdout)
		})
	case (len(args) == 4 || len(args) == 5) && args[0] == "api-key" && args[1] == "add":
		projectID := ""
		if len(args) == 5 {
			projectID = args[4]
		}
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			return addAPIKey(ctx, db.NewAccountsStore(pool), args[2], args[3], projectID, stdout)
		})
	case len(args) == 4 && args[0] == "user" && args[1] == "staff":
		staff, err := onOff(args[3])
		if err != nil {
			return err
		}
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			return db.NewAccountsStore(pool).SetUserStaff(ctx, args[2], staff)
		})
	case len(args) == 4 && args[0] == "cluster" && args[1] == "share":
		shared, err := onOff(args[3])
		if err != nil {
			return err
		}
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			_, requeued, err := db.NewClustersStore(pool).SetClusterShared(ctx, args[2], shared)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(struct {
				Requeued []string `json:"requeued"`
			}{append([]string{}, requeued...)})
		})
	case len(args) == 3 && args[0] == "platform-experiment" && (args[1] == "share" || args[1] == "unshare"):
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			found, err := db.NewPlatformExperimentsStore(pool).SetPlatformExperimentShared(ctx, args[2], args[1] == "share")
			if err == nil && !found {
				err = fmt.Errorf("no platform experiment %s", args[2])
			}
			return err
		})
	case len(args) == 4 && args[0] == "cluster" && args[1] == "add":
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			if _, err := db.NewClustersStore(pool).RegisterCluster(ctx, args[2], args[3]); err != nil {
				return err
			}
			token, err := auth.Mint(cfg.Auth.SigningSecret, auth.RuntimeScope(args[2]))
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, token)
			return nil
		})
	case len(args) == 3 && args[0] == "launcher" && args[1] == "add":
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			if err := db.NewPlatformExperimentsStore(pool).RegisterLauncher(ctx, args[2]); err != nil {
				return err
			}
			token, err := auth.Mint(cfg.Auth.SigningSecret, auth.LauncherScope(args[2]))
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, token)
			return nil
		})
	case len(args) == 3 && args[0] == "launcher" && args[1] == "remove":
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			removed, err := db.NewPlatformExperimentsStore(pool).RemoveLauncher(ctx, args[2])
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("no launcher %s is registered", args[2])
			}
			return nil
		})
	case len(args) == 4 && args[0] == "github" && args[1] == "connect":
		installation, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil || installation <= 0 {
			return fmt.Errorf("INSTALLATION_ID must be the installation's number, got %q", args[3])
		}
		if !cfg.GitHub.Configured() {
			return errors.New("the config has no github section, so there is no App to ask about the installation")
		}
		app, err := github.New(cfg.GitHub)
		if err != nil {
			return err
		}
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			return connectGitHub(ctx, db.NewAccountsStore(pool), app, args[2], installation, stdout)
		})
	default:
		return errors.New(usage)
	}
}

func onOff(arg string) (bool, error) {
	switch arg {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("want on or off, got %q", arg)
	}
}

func loadConfig() (*config.Config, error) {
	path := os.Getenv("HYPOTHESISLOOP_CONFIG")
	if path == "" {
		return nil, errors.New("HYPOTHESISLOOP_CONFIG environment variable is required")
	}
	return config.Load(path)
}

type installationStore interface {
	ConnectGitHubInstallation(ctx context.Context, installation *domain.OrgGitHubInstallation) error
}

type installations interface {
	Installation(ctx context.Context, installationID int64) (*github.Installation, error)
}

// connectedByOperator is who connected an installation that no person proved they reach.
const connectedByOperator = "hl-admin"

// connectGitHub records an installation as an org's. Nothing typed is recorded unchecked: the
// installation is the App's own, and the account it is on is GitHub's answer.
func connectGitHub(ctx context.Context, store installationStore, app installations, orgID string, installationID int64, stdout io.Writer) error {
	found, err := app.Installation(ctx, installationID)
	if err != nil {
		return fmt.Errorf("installation %d: %w", installationID, err)
	}
	connected := &domain.OrgGitHubInstallation{OrgID: orgID, InstallationID: found.ID, AccountLogin: found.AccountLogin, ConnectedBy: connectedByOperator}
	if err := store.ConnectGitHubInstallation(ctx, connected); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(connected)
}

func withPool(do func(context.Context, *db.Pool) error) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL environment variable is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	return do(ctx, pool)
}

type userHomes interface {
	EnsureUserHome(ctx context.Context, userID, name, orgName string) (*domain.User, []string, error)
	ListProjects(ctx context.Context, orgIDs []string) ([]*domain.Project, error)
}

func addUser(ctx context.Context, accounts userHomes, subject, name string, stdout io.Writer) error {
	user, orgIDs, err := accounts.EnsureUserHome(ctx, subject, name, name)
	if err != nil {
		return err
	}
	if len(orgIDs) == 0 {
		return fmt.Errorf("user %s exists and belongs to no org: add them to one before asking for their home", subject)
	}
	projects, err := accounts.ListProjects(ctx, orgIDs[:1])
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		return fmt.Errorf("org %s of user %s has no project", orgIDs[0], subject)
	}
	return json.NewEncoder(stdout).Encode(struct {
		UserID    string `json:"user_id"`
		OrgID     string `json:"org_id"`
		ProjectID string `json:"project_id"`
	}{user.ID, orgIDs[0], projects[0].ID})
}

func addAPIKey(ctx context.Context, accounts *db.AccountsStore, subject, orgID, projectID string, stdout io.Writer) error {
	id, token, err := auth.NewAPIKey()
	if err != nil {
		return err
	}
	key := &domain.APIKey{ID: id, OrgID: orgID, ProjectID: projectID, CreatedBy: subject, Name: "hl-admin"}
	if err := accounts.CreateAPIKey(ctx, key, auth.HashAPIKey(token)); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}{id, token})
}
