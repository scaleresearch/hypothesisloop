// hl-admin is the operator's hand on tenancy for a deployment that has the signing secret and
// the database: it mints derived tokens and registers users, API keys and clusters. Local
// development and the e2e suite set themselves up with it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const usage = `usage: hl-admin <command>

  token SCOPE                      print the token for a scope: staff, runtime:<cluster>,
                                   agent:<platform experiment>/<agent>, coordinator:<platform experiment>,
                                   job:<experiment>, launcher:<name>
  user add SUBJECT NAME            create a user with their home org and its default project
  api-key add SUBJECT ORG [PROJECT]  create an API key of ORG, bound to PROJECT when given
  cluster add NAME ORG             register a cluster for ORG and print its runtime token

token reads HYPOTHESISLOOP_CONFIG. The others also read DATABASE_URL.`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hl-admin: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	cfg, err := loadAuthConfig()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 2 && args[0] == "token":
		token, err := auth.Mint(cfg.SigningSecret, args[1])
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
	case len(args) == 4 && args[0] == "cluster" && args[1] == "add":
		return withPool(func(ctx context.Context, pool *db.Pool) error {
			if _, err := db.NewClustersStore(pool).RegisterCluster(ctx, args[2], args[3]); err != nil {
				return err
			}
			token, err := auth.Mint(cfg.SigningSecret, auth.RuntimeScope(args[2]))
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, token)
			return nil
		})
	default:
		return errors.New(usage)
	}
}

func loadAuthConfig() (config.AuthConfig, error) {
	path := os.Getenv("HYPOTHESISLOOP_CONFIG")
	if path == "" {
		return config.AuthConfig{}, errors.New("HYPOTHESISLOOP_CONFIG environment variable is required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.AuthConfig{}, err
	}
	return cfg.Auth, nil
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

func addUser(ctx context.Context, accounts *db.AccountsStore, subject, name string, stdout io.Writer) error {
	user, orgIDs, err := accounts.EnsureUserHome(ctx, subject, name)
	if err != nil {
		return err
	}
	projects, err := accounts.ListProjects(ctx, orgIDs[:1])
	if err != nil {
		return err
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
