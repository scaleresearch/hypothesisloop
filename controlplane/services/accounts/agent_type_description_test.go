package accounts

import (
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestADescriptionIsKeptOnCreateAndEditAndCopiedWithTheType(t *testing.T) {
	w := newWorld(t)
	var list []domain.CurrentAgentType
	w.ok("member", "GET", "/orgs/"+w.orgA+"/agent-types", nil, http.StatusOK, &list)
	for _, ty := range list {
		if ty.Builtin && ty.Description == "" {
			t.Errorf("built-in %s lists no description", ty.ID)
		}
	}
	var made domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "Mine", "description": "Tunes the rate.", "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, &made)
	if made.Description != "Tunes the rate." {
		t.Fatalf("created: got description %q", made.Description)
	}
	var read domain.CurrentAgentType
	w.ok("member", "GET", "/orgs/"+w.orgA+"/agent-types/"+made.ID, nil, http.StatusOK, &read)
	if read.Description != "Tunes the rate." {
		t.Errorf("read: got description %q", read.Description)
	}
	var edited EditedAgentType
	w.ok("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+made.ID, name{"if_version": 1, "name": "Mine", "description": "Tunes the batch size.", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusOK, &edited)
	if edited.Description != "Tunes the batch size." {
		t.Errorf("edited: got description %q", edited.Description)
	}
	w.ok("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+made.ID, name{"if_version": 2, "name": "Mine", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusOK, &edited)
	if edited.Description != "Tunes the batch size." {
		t.Errorf("an edit without description: got description %q, want it kept", edited.Description)
	}
	var copied domain.CurrentAgentType
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"from": "generalist"}, http.StatusCreated, &copied)
	if copied.Description != w.agentTypes.types["generalist"].Description || copied.Description == "" {
		t.Errorf("copy: got description %q, want the generalist's", copied.Description)
	}
}

func TestADescriptionOfMoreThanOneLineOrTwoHundredCharactersIsRefused(t *testing.T) {
	w := newWorld(t)
	for what, description := range map[string]string{
		"two lines": "one\ntwo",
		"too long":  strings.Repeat("x", domain.MaxAgentTypeDescriptionLength+1),
	} {
		body := name{"name": "x", "description": description, "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}
		if rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/agent-types", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("create, %s: got = %d %s, want 422", what, rec.Code, rec.Body)
		}
		edit := name{"if_version": 1, "name": "Own", "description": description, "prompt": ownPrompt, "settings": ownSettings()}
		if rec := w.call("owner", "PUT", "/orgs/"+w.orgA+"/agent-types/"+w.ownType, edit); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("edit, %s: got = %d %s, want 422", what, rec.Code, rec.Body)
		}
	}
	exact := strings.Repeat("x", domain.MaxAgentTypeDescriptionLength)
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/agent-types", name{"name": "x", "description": exact, "role": "researcher", "prompt": ownPrompt, "settings": ownSettings()}, http.StatusCreated, nil)
}
