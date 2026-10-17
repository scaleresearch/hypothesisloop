package e2b

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// templateIdle is how long a template of the platform goes without a sandbox before it is deleted.
// A job that needs it again has it built again.
const templateIdle = 48 * time.Hour

// platformTemplate is the form of every name templateName makes.
var platformTemplate = regexp.MustCompile(`\Ahl-[0-9a-f]{20}-[0-9]+c-[0-9]+m\z`)

// prune deletes the templates of the platform that no running or paused sandbox was started
// from and that have had no sandbox for templateIdle. It runs when a new template is built, which
// is what adds one, so templates of digests no longer used do not pile up. A template is the
// platform's when every name it has is of templateName's form. What fails is logged and tried
// again at the next build.
func (p *Provider) prune(ctx context.Context, templates []vendorTemplate) {
	sandboxes, err := p.allSandboxes(ctx)
	if err != nil {
		p.log("e2b: old templates are kept: the sandboxes that may use them could not be listed: %v", err)
		return
	}
	inUse := map[string]bool{}
	for _, s := range sandboxes {
		inUse[s.TemplateID], inUse[s.Alias] = true, true
	}
	for _, t := range templates {
		used := inUse[t.TemplateID] || slices.ContainsFunc(t.aliases(), func(alias string) bool { return inUse[alias] })
		if used || !platformOwned(t) || time.Since(lastUsed(t)) < templateIdle {
			continue
		}
		if _, err := p.call(ctx, http.MethodDelete, "/templates/"+url.PathEscape(t.TemplateID), nil, nil); err != nil {
			p.log("e2b: deleting unused template %v (%s): %v", t.aliases(), t.TemplateID, err)
			continue
		}
		p.log("e2b: deleted template %v (%s), unused since %s", t.aliases(), t.TemplateID, lastUsed(t).Format(time.RFC3339))
	}
}

func platformOwned(t vendorTemplate) bool {
	aliases := t.aliases()
	return len(aliases) > 0 && !slices.ContainsFunc(aliases, func(alias string) bool { return !platformTemplate.MatchString(alias) })
}

func lastUsed(t vendorTemplate) time.Time {
	if t.LastSpawnedAt != nil && t.LastSpawnedAt.After(t.CreatedAt) {
		return *t.LastSpawnedAt
	}
	return t.CreatedAt
}
