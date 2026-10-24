package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const testPrompt = "{{rules}}\n{{hl_commands}}\nYou are {{agent_id}} in {{platform_experiment_id}}."

func testTypeSettings() domain.AgentTypeSettings {
	return domain.AgentTypeSettings{MaxSubagents: 4, Effort: "high", Hyperparameters: map[string]any{}}
}

// orgWithExperiment is a new org, a platform experiment of it with max_agents 10, and its own
// executor type.
func orgWithExperiment(t *testing.T, pool *Pool) (orgID, peID string, custom *domain.CurrentAgentType) {
	t.Helper()
	accounts := NewStore(pool, 3)
	_, orgID = newTestUserHome(t, accounts)
	peID = "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, peID, defaultProjectOf(t, accounts, orgID).ID)
	custom, err := NewStore(pool, 3).Create(context.Background(), orgID, "Tuner", "Tunes the knobs.", domain.AgentRoleExecutor, testPrompt, testTypeSettings(), domain.Actor{Kind: domain.ActorUser, ID: "u"})
	if err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	return orgID, peID, custom
}

func signupOf(t *testing.T, pes *Store, peID, agentID string) *domain.AgentSignup {
	t.Helper()
	signup, err := pes.GetAgentSignup(context.Background(), peID, agentID)
	if err != nil || signup == nil {
		t.Fatalf("signup %s: got = %v, %v", agentID, signup, err)
	}
	return signup
}

func actions(t *testing.T, store *Store, orgID, id string) []string {
	t.Helper()
	events, err := store.Events(context.Background(), orgID, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s@%d", e.Action, e.Version))
	}
	return out
}

// An edit inserts the next version and leaves the one before as it was. No write of the store
// updates or deletes a version or an event (TestEveryAnnouncedWriteAnnouncesAndAppendOnlyTablesAreOnlyAppended).
func TestAnEditKeepsEveryVersionAsItWas(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	orgID, _, custom := orgWithExperiment(t, pool)

	edited, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Description: custom.Description, Prompt: testPrompt + " Tune.", Settings: testTypeSettings()})
	if err != nil || edited.CurrentVersion != 2 || edited.Current.Prompt != testPrompt+" Tune." {
		t.Fatalf("edit: got = %+v, %v, want version 2", edited, err)
	}
	first, err := store.Version(ctx, orgID, custom.ID, 1)
	if err != nil || first.Prompt != testPrompt || first.Digest != custom.Current.Digest || first.Release != nil {
		t.Errorf("version 1 after the edit: got = %+v, %v, want it as it was, of no release", first, err)
	}
	versions, err := store.Versions(ctx, orgID, custom.ID)
	if err != nil || len(versions) != 2 || versions[0].Version != 2 {
		t.Errorf("versions: got = %+v, %v, want 2 then 1", versions, err)
	}
	if again, propagation, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 2, Name: "Tuner 2", Description: custom.Description, Prompt: testPrompt + " Tune.", Settings: testTypeSettings()}); err != nil || again.CurrentVersion != 2 || again.Name != "Tuner 2" || propagation.Version != 2 {
		t.Errorf("an edit of the name alone: got = %+v, %+v, %v, want the name changed and no version", again, propagation, err)
	}
	if got := actions(t, store, orgID, custom.ID); !slices.Equal(got, []string{"rename@2", "edit@2", "create@1"}) {
		t.Errorf("events: got = %v, want create, edit, rename", got)
	}
}

// A release writes what it holds whenever it differs from the current version and the release is
// that version's or newer: a revert shipped under a newer release becomes current, and an older
// release, as an older replica of a rolling deploy has, writes nothing.
func TestSeedingWritesWhatDiffersFromTheCurrentVersionUnlessAnOlderReleaseHasIt(t *testing.T) {
	pool := builtDatabase(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	builtin := domain.BuiltinAgentType{ID: "test-" + uuid.New().String()[:8], Name: "Test", Role: domain.AgentRoleExecutor, Prompt: testPrompt, Settings: testTypeSettings()}
	seed := func(release int, b domain.BuiltinAgentType) Seeded {
		t.Helper()
		seeded, err := NewStore(pool, 3).SeedBuiltins(ctx, release, []domain.BuiltinAgentType{b}, testRules)
		if err != nil {
			t.Fatalf("seed: got = %v, want = nil", err)
		}
		return seeded
	}
	current := func() *domain.CurrentAgentType {
		t.Helper()
		got, err := store.Get(ctx, "", builtin.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	seed(5, builtin)
	if got := current(); got.CurrentVersion != 1 || !got.Builtin || got.OrgID != "" || *got.Current.Release != 5 {
		t.Fatalf("first seed: got = %+v, want a built-in at version 1 of release 5", got)
	}
	if seeded := seed(5, builtin); len(seeded.Types) != 0 || current().CurrentVersion != 1 {
		t.Errorf("a second seed of the same: seeded %+v, version %d, want nothing", seeded, current().CurrentVersion)
	}
	changed := builtin
	changed.Prompt += " Changed."
	changed.Name = "Test, changed"
	if seeded := seed(6, changed); len(seeded.Types) != 1 || seeded.Types[0].Propagation.Version != 2 || current().CurrentVersion != 2 {
		t.Errorf("a changed prompt: seeded %+v, version %d, want version 2", seeded, current().CurrentVersion)
	}
	if seeded := seed(5, builtin); len(seeded.Types) != 0 || !slices.Contains(seeded.Skipped, builtin.ID) || current().CurrentVersion != 2 || current().Name != "Test, changed" {
		t.Errorf("the older release again: seeded %+v, got %+v, want version 2 and its name kept", seeded, current())
	}
	if seeded := seed(7, builtin); len(seeded.Types) != 1 || current().CurrentVersion != 3 || current().Current.Prompt != testPrompt || current().Name != "Test" {
		t.Errorf("the revert under release 7: seeded %+v, got %+v, want version 3 with the first prompt", seeded, current())
	}
	other := changed
	other.Role = domain.AgentRoleBaseline
	if _, err := NewStore(pool, 3).SeedBuiltins(ctx, 7, []domain.BuiltinAgentType{other}, testRules); err == nil {
		t.Error("a built-in seeded with another role was accepted")
	}
	if got := actions(t, store, "", builtin.ID); !slices.Equal(got, []string{"edit@3", "rename@2", "edit@2", "rename@1", "create@1"}) {
		t.Errorf("events: got = %v", got)
	}
}

// The platform's rules are versioned as a built-in is, and a new version tells every live agent
// and coordinator, whose instructions hold it and its version from then on.
func TestNewRulesAreAVersionEveryLiveAgentIsToldOf(t *testing.T) {
	pool := builtDatabase(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	_, peID, custom := orgWithExperiment(t, pool)
	ids := spawnTestAgents(t, pes, peID, domain.AgentSignup{Actor: testActor, AgentType: custom.ID})
	before, err := pes.AgentInstructions(ctx, peID, ids[0])
	if err != nil || before.Rules != testRules {
		t.Fatalf("before: got = %+v, %v", before, err)
	}
	live, stop := listenForEvents(t, pool)
	defer stop()

	const release = 1000
	seeded, err := NewStore(pool, 3).SeedBuiltins(ctx, release, nil, "## new rules")
	if err != nil || seeded.RulesVersion != before.RulesVersion+1 || seeded.RulesTold < 2 {
		t.Fatalf("new rules: got = %+v, %v, want the next version and the agent and coordinator told", seeded, err)
	}
	told := map[string]bool{}
	for deadline := time.After(5 * time.Second); len(told) < 2; {
		select {
		case e := <-live:
			if e.Kind == platformapi.EventAgentInstructions && e.PlatformExperimentID == peID {
				told[e.AgentID] = true
			}
		case <-deadline:
			t.Fatalf("told: got = %v, want the agent and the coordinator", told)
		}
	}
	after, err := pes.AgentInstructions(ctx, peID, ids[0])
	if err != nil || after.Rules != "## new rules" || after.RulesVersion != seeded.RulesVersion {
		t.Errorf("after: got = %+v, %v", after, err)
	}
	if seeded, err := NewStore(pool, 3).SeedBuiltins(ctx, release-1, nil, testRules); err != nil || seeded.RulesVersion != 0 || !slices.Contains(seeded.Skipped, "rules") {
		t.Errorf("an older release's rules: got = %+v, %v, want them left alone", seeded, err)
	}
	if seeded, err := NewStore(pool, 3).SeedBuiltins(ctx, release, nil, testRules); err != nil || seeded.RulesVersion != after.RulesVersion+1 {
		t.Errorf("the rules back under the same release: got = %+v, %v, want the next version", seeded, err)
	}
}

func TestAnOrgsTypeIsItsAloneAndABuiltinIsNobodys(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	orgID, _, custom := orgWithExperiment(t, pool)
	_, other := newTestUserHome(t, NewStore(pool, 3))

	if _, err := store.Get(ctx, other, custom.ID); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org reads it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: other, ID: custom.ID, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org edits it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, _, err := store.Restore(ctx, other, custom.ID, 1, *testActor); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org restores it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, err := store.Archive(ctx, other, custom.ID, *testActor); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org archives it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, err := store.Events(ctx, other, custom.ID); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org reads its events: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: domain.DefaultAgentType, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}); !errors.Is(err, ErrAgentTypeBuiltin) {
		t.Errorf("an org edits a built-in: got = %v, want ErrAgentTypeBuiltin", err)
	}
	if _, _, err := store.Restore(ctx, orgID, domain.DefaultAgentType, 1, *testActor); !errors.Is(err, ErrAgentTypeBuiltin) {
		t.Errorf("an org restores a built-in: got = %v, want ErrAgentTypeBuiltin", err)
	}
	if _, err := store.Archive(ctx, orgID, domain.DefaultAgentType, *testActor); !errors.Is(err, ErrAgentTypeBuiltin) {
		t.Errorf("an org archives a built-in: got = %v, want ErrAgentTypeBuiltin", err)
	}
	var conflict *AgentTypeVersionConflict
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 7, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}); !errors.As(err, &conflict) || conflict.Current != 1 {
		t.Errorf("an edit from a stale version: got = %v, want a conflict naming version 1", err)
	}
	listed, err := store.List(ctx, orgID)
	if err != nil || !slices.ContainsFunc(listed, func(t domain.CurrentAgentType) bool { return t.ID == custom.ID }) || !slices.ContainsFunc(listed, func(t domain.CurrentAgentType) bool { return t.ID == "coordinator" && t.Builtin }) {
		t.Errorf("the org's list: got = %v, %v, want its own and the built-ins", listed, err)
	}
	if listed, _ := store.List(ctx, other); slices.ContainsFunc(listed, func(t domain.CurrentAgentType) bool { return t.ID == custom.ID }) {
		t.Error("another org lists it")
	}
}

func TestARestoreMakesAnEarlierVersionCurrentAgain(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	orgID, _, custom := orgWithExperiment(t, pool)
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: custom.Name, Description: custom.Description, Prompt: testPrompt + " Two.", Settings: testTypeSettings()}); err != nil {
		t.Fatal(err)
	}
	restored, propagation, err := store.Restore(ctx, orgID, custom.ID, 1, *testActor)
	if err != nil || restored.CurrentVersion != 3 || restored.Current.Prompt != testPrompt || restored.Current.Digest != custom.Current.Digest || propagation.Version != 3 {
		t.Fatalf("restore 1: got = %+v, %+v, %v, want version 3 holding version 1", restored, propagation, err)
	}
	if again, propagation, err := store.Restore(ctx, orgID, custom.ID, 1, *testActor); err != nil || again.CurrentVersion != 3 || propagation.Version != 3 {
		t.Errorf("restore what is current: got = %+v, %+v, %v, want no version", again, propagation, err)
	}
	if _, _, err := store.Restore(ctx, orgID, custom.ID, 9, *testActor); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("restore a version it lacks: got = %v, want ErrAgentTypeNotFound", err)
	}
	events, err := store.Events(ctx, orgID, custom.ID)
	if err != nil || events[0].Action != domain.AgentTypeRestored || events[0].Version != 3 || events[0].Detail != "version 1" || events[0].By == nil {
		t.Errorf("events: got = %+v, %v, want the restore first", events, err)
	}
}

func TestAnOrgsTypesAndTheirEditsAreBounded(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	orgID, _, custom := orgWithExperiment(t, pool)
	var limit *AgentTypeLimit
	for n := 1; n < domain.MaxOrgAgentTypes; n++ {
		if _, err := store.Create(ctx, orgID, fmt.Sprintf("Type %d", n), "", domain.AgentRoleExecutor, testPrompt, testTypeSettings(), *testActor); err != nil {
			t.Fatalf("type %d: %v", n+1, err)
		}
	}
	if _, err := store.Create(ctx, orgID, "One too many", "", domain.AgentRoleExecutor, testPrompt, testTypeSettings(), *testActor); !errors.As(err, &limit) || !limit.Types {
		t.Errorf("type %d: got = %v, want the limit on types", domain.MaxOrgAgentTypes+1, err)
	}
	for n := 1; n < domain.MaxAgentTypeEditsAnHour; n++ {
		if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: n, Name: custom.Name, Description: custom.Description, Prompt: fmt.Sprintf("%s %d", testPrompt, n), Settings: testTypeSettings()}); err != nil {
			t.Fatalf("edit %d: %v", n, err)
		}
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: domain.MaxAgentTypeEditsAnHour, Name: custom.Name, Description: custom.Description, Prompt: testPrompt + " more", Settings: testTypeSettings()}); !errors.As(err, &limit) || limit.Types {
		t.Errorf("edit %d: got = %v, want the limit on edits", domain.MaxAgentTypeEditsAnHour, err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: domain.MaxAgentTypeEditsAnHour, Name: "Renamed", Description: custom.Description, Prompt: fmt.Sprintf("%s %d", testPrompt, domain.MaxAgentTypeEditsAnHour-1), Settings: testTypeSettings()}); err != nil {
		t.Errorf("a rename past the limit: got = %v, want it written, as it makes no version", err)
	}
}

func TestATypeInUseIsNotArchivedAndAnArchivedOneTakesNoAgent(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	pes := NewStore(pool, 3)
	orgID, peID, custom := orgWithExperiment(t, pool)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET agents = jsonb_build_array(jsonb_build_object('agent_type', $2::text, 'model', 'm', 'count', 1)) WHERE id = $1`, peID, custom.ID); err != nil {
		t.Fatal(err)
	}
	var inUse *AgentTypeInUse
	if _, err := store.Archive(ctx, orgID, custom.ID, *testActor); !errors.As(err, &inUse) || !slices.Equal(inUse.PlatformExperimentIDs, []string{peID}) {
		t.Fatalf("archive while a pool names it: got = %v, want in use by %s", err, peID)
	}
	setPEStatus(t, pool, peID, domain.PlatformExpClosed, time.Minute)
	archived, err := store.Archive(ctx, orgID, custom.ID, *testActor)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive once its experiment closed: got = %+v, %v", archived, err)
	}
	if _, err := store.Archive(ctx, orgID, custom.ID, *testActor); err != nil {
		t.Errorf("archive again: got = %v", err)
	}
	if got := actions(t, store, orgID, custom.ID); !slices.Equal(got, []string{"archive@1", "create@1"}) {
		t.Errorf("events: got = %v, want one archive", got)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}); !errors.Is(err, ErrAgentTypeArchived) {
		t.Errorf("edit an archived type: got = %v, want ErrAgentTypeArchived", err)
	}
	open := "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, open, defaultProjectOf(t, NewStore(pool, 3), orgID).ID)
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:8], CreatedAt: time.Now()}
	if err := pes.SpawnAgent(ctx, open, agent, domain.AgentSignup{Actor: testActor, AgentID: agent.ID, Role: domain.SignupRoleCompetitor, AgentType: custom.ID, Model: "m"}); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("an agent of an archived type: got = %v, want ErrUnknownAgentType", err)
	}
}

// The write that names an agent type holds its row FOR SHARE until it commits, so an archive
// waits for it and then sees it in use.
func TestAWriteNamingATypeHoldsItAgainstAnArchive(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, custom := orgWithExperiment(t, pool)
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := agentTypeTx(ctx, tx, peID, custom.ID, domain.AgentRoleExecutor); err != nil {
		t.Fatal(err)
	}
	archive, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Rollback(ctx) //nolint:errcheck
	if _, err := archive.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Exec(ctx, `SELECT 1 FROM agent_types WHERE id = $1 FOR UPDATE`, custom.ID); err == nil {
		t.Error("the type was locked for an archive while a write naming it was open")
	}
}

func TestAnAgentRunsAnAgentTypeOfItsOrgAndItsRole(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	_, peID, custom := orgWithExperiment(t, pool)
	_, _, foreign := orgWithExperiment(t, pool)

	spawn := func(role domain.SignupRole, agentType, model string) (string, error) {
		agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:8], CreatedAt: time.Now()}
		return agent.ID, pes.SpawnAgent(ctx, peID, agent, domain.AgentSignup{Actor: testActor, AgentID: agent.ID, Role: role, AgentType: agentType, Model: model})
	}
	own, err := spawn(domain.SignupRoleCompetitor, custom.ID, "m")
	if err != nil {
		t.Fatalf("an agent of its org's type: got = %v, want = nil", err)
	}
	if got := signupOf(t, pes, peID, own); got.AgentType != custom.ID || got.Model != "m" || got.Hyperparameters != nil {
		t.Errorf("its signup: got = %+v, want %s on m with its type's hyperparameters", got, custom.ID)
	}
	follows, err := spawn(domain.SignupRoleCompetitor, custom.ID, "")
	if err != nil || signupOf(t, pes, peID, follows).Model != "" {
		t.Errorf("an agent naming no model: got = %v, want its signup holding none", err)
	}
	if _, err := spawn(domain.SignupRoleCompetitor, foreign.ID, ""); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("another org's type: got = %v, want ErrUnknownAgentType", err)
	}
	if _, err := spawn(domain.SignupRoleBaseline, custom.ID, ""); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("an executor type for a baseline agent: got = %v, want ErrUnknownAgentType", err)
	}
	baseline, err := spawn(domain.SignupRoleBaseline, "", "")
	if err != nil || signupOf(t, pes, peID, baseline).AgentType != "baseline" {
		t.Errorf("a baseline agent naming no type: got = %v, want the built-in baseline", err)
	}
	src, err := pes.AgentInstructions(ctx, peID, own)
	if err != nil || src == nil || src.Role != domain.AgentRoleExecutor || src.Version.Prompt != testPrompt || src.Model != "m" || src.Hyperparameters != nil || src.Rules == "" || src.RulesVersion < 1 {
		t.Errorf("what its instructions are composed from: got = %+v, %v", src, err)
	}
	if src, err := pes.AgentInstructions(ctx, peID, "ag-nobody"); err != nil || src != nil {
		t.Errorf("an agent not in it: got = %+v, %v, want none", src, err)
	}
	coordinator, err := pes.CoordinatorInstructions(ctx, peID)
	if err != nil || coordinator == nil || coordinator.Role != domain.AgentRoleCoordinator || coordinator.Version.TypeID != "coordinator" {
		t.Errorf("the coordinator's: got = %+v, %v, want the built-in coordinator", coordinator, err)
	}
}

// An edit makes the next version current: every agent of the type runs it, those that name no
// model or hyperparameters of their own on the type's new ones, and each whose run goes on is told.
func TestAnEditIsWhatEveryAgentOfTheTypeRunsAndEachLiveOneIsTold(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	pes := NewStore(pool, 3)
	orgID, peID, custom := orgWithExperiment(t, pool)
	ids := spawnTestAgents(t, pes, peID,
		domain.AgentSignup{Actor: testActor, AgentType: custom.ID},
		domain.AgentSignup{Actor: testActor, AgentType: custom.ID, Model: "its-own", Hyperparameters: map[string]any{"lr": 1.0}},
		domain.AgentSignup{Actor: testActor, AgentType: custom.ID})
	follows, own, finished := ids[0], ids[1], ids[2]
	setPEStatus(t, pool, peID, domain.PlatformExpRunning, time.Minute)
	if err := pes.FinishAgent(ctx, peID, finished, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET agents = jsonb_build_array(jsonb_build_object('agent_type', $2::text, 'count', 3)) WHERE id = $1`, peID, custom.ID); err != nil {
		t.Fatal(err)
	}
	since := cursorNow(t, pool)

	settings := testTypeSettings()
	settings.Model, settings.Hyperparameters = "the-new-model", map[string]any{"lr": 0.5}
	edited, propagation, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Description: custom.Description, Prompt: testPrompt, Settings: settings})
	if err != nil || edited.CurrentVersion != 2 || propagation.Version != 2 || propagation.Told != 2 {
		t.Fatalf("edit: got = %+v, %+v, %v, want version 2 told to the two live agents", edited, propagation, err)
	}
	for _, c := range []struct {
		agent, model string
		lr           any
	}{{follows, "", nil}, {own, "its-own", 1.0}} {
		src, err := pes.AgentInstructions(ctx, peID, c.agent)
		if err != nil || src.Version.Version != 2 || src.Version.Settings.Model != "the-new-model" || src.Model != c.model || (c.lr != nil && src.Hyperparameters["lr"] != c.lr) || (c.lr == nil && src.Hyperparameters != nil) {
			t.Errorf("%s: got = %+v, %v, want version 2 with model %q of its own", c.agent, src, err, c.model)
		}
	}
	pe, err := pes.GetPlatformExperiment(ctx, peID)
	if err != nil || len(pe.Agents) != 1 || pe.Agents[0].Model != "" {
		t.Errorf("the pool: got = %+v, %v, want its entry still naming no model", pe.Agents, err)
	}

	instructions := map[string]bool{platformapi.EventAgentInstructions: true}
	told := map[string]string{}
	for _, e := range changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Kinds: instructions}, since) {
		if e.AgentID != domain.CoordinatorRecipient {
			if _, again := told[e.AgentID]; again {
				t.Errorf("%s told twice", e.AgentID)
			}
			told[e.AgentID] = e.Value + "/" + e.Detail
		}
	}
	if len(told) != 2 || told[follows] != "2/"+custom.ID || told[own] != "2/"+custom.ID {
		t.Errorf("told: got = %v, want version 2 of %s to each live one", told, custom.ID)
	}
	var ownTold []domain.Event
	for _, e := range changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, AgentID: own}, since) {
		if e.Kind == platformapi.EventAgentInstructions {
			ownTold = append(ownTold, e)
		}
	}
	if len(ownTold) != 1 || ownTold[0].AgentID != own || ownTold[0].Value != "2" {
		t.Errorf("told to %s: got = %+v, want its own one", own, ownTold)
	}
}

func TestAnEditOfACoordinatorTypeTellsTheCoordinatorsOfRunsThatGoOn(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	pes := NewStore(pool, 3)
	orgID, peID, _ := orgWithExperiment(t, pool)
	coordinatorPrompt := "{{rules}}\n{{hl_commands}}\nCoordinate {{platform_experiment_id}}."
	custom, err := store.Create(ctx, orgID, "Strict coordinator", "", domain.AgentRoleCoordinator, coordinatorPrompt, testTypeSettings(), domain.Actor{Kind: domain.ActorUser, ID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET coordinator_agent_type = $2, coordinator_model = NULL WHERE id = $1`, peID, custom.ID); err != nil {
		t.Fatal(err)
	}
	settings := testTypeSettings()
	settings.Model = "coordinator-next"
	since := cursorNow(t, pool)
	_, propagation, err := store.Edit(ctx, AgentTypeEdit{By: *testActor, OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: custom.Name, Prompt: coordinatorPrompt + " Strictly.", Settings: settings})
	if err != nil || propagation.Told != 1 {
		t.Fatalf("edit: got = %+v, %v, want the coordinator of %s told", propagation, err, peID)
	}
	src, err := pes.CoordinatorInstructions(ctx, peID)
	if err != nil || src.Version.Version != 2 || src.Model != "" || src.Version.Settings.Model != "coordinator-next" {
		t.Errorf("the coordinator: got = %+v, %v, want version 2 on the type's new model", src, err)
	}
	told := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventAgentInstructions: true}}, since)
	if len(told) != 1 || told[0].AgentID != domain.CoordinatorRecipient || told[0].Value != "2" {
		t.Errorf("told: got = %+v, want agent.instructions to the coordinator, once", told)
	}
}
