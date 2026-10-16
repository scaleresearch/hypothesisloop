package db

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// tenantClause restricts rows to the platform experiments inside scope, matched on peColumn, and
// not removed. It returns "" for an unrestricted scope, and otherwise a predicate numbered after
// args.
func tenantClause(scope domain.TenantScope, peColumn string, args []any) (string, []any) {
	if scope.Unrestricted() {
		return "", args
	}
	conds := []string{"tpe.removed_at IS NULL"}
	if !scope.AllOrgs {
		args = append(args, scope.OrgIDs)
		conds = append(conds, fmt.Sprintf("tp.org_id = ANY($%d)", len(args)))
	}
	if scope.ProjectID != "" {
		args = append(args, scope.ProjectID)
		conds = append(conds, fmt.Sprintf("tpe.project_id = $%d", len(args)))
	}
	if scope.PlatformExperimentID != "" {
		args = append(args, scope.PlatformExperimentID)
		conds = append(conds, fmt.Sprintf("tpe.id = $%d", len(args)))
	}
	return peColumn + ` IN (SELECT tpe.id FROM platform_experiments tpe JOIN projects tp ON tp.id = tpe.project_id WHERE ` +
		strings.Join(conds, " AND ") + `)`, args
}
