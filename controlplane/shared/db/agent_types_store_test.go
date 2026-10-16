package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const testPrompt = "{{rules}}\n{{hl_commands}}\nYou are {{agent_id}} in {{platform_experiment_id}}."

func testTypeSettings() domain.AgentTypeSettings {
	return domain.AgentTypeSettings{MaxSubagents: 4, PacingSubagents: 2, Effort: "high", Hyperparameters: map[string]any{}}
}

// orgWithExperiment is a new org, a platform experiment of it with max_agents 10, and its own
// researcher type.
func orgWithExperiment(t *testing.T, pool *Pool) (orgID, peID string, custom *domain.CurrentAgentType) {
	t.Helper()
	accounts := NewAccountsStore(pool)
	_, orgID = newTestUserHome(t, accounts)
	peID = "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, peID, defaultProjectOf(t, accounts, orgID).ID)
	custom, err := NewAgentTypesStore(pool).Create(context.Background(), orgID, "Tuner", domain.AgentRoleResearcher, testPrompt, testTypeSettings(), domain.Actor{Kind: domain.ActorUser, ID: "u"})
	if err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	return orgID, peID, custom
}

func signupOf(t *testing.T, pes *PlatformExperimentsStore, peID, agentID string) *domain.AgentSignup {
	t.Helper()
	signup, err := pes.GetAgentSignup(context.Background(), peID, agentID)
	if err != nil || signup == nil {
		t.Fatalf("signup %s: got = %v, %v", agentID, signup, err)
	}
	return signup
}

func TestAVersionIsNeverChangedOrDeleted(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	orgID, _, custom := orgWithExperiment(t, pool)

	if _, err := pool.pool.Exec(ctx, `UPDATE agent_type_versions SET prompt = 'x' WHERE type_id = $1`, custom.ID); err == nil {
		t.Error("a version was updated")
	}
	if _, err := pool.pool.Exec(ctx, `DELETE FROM agent_type_versions WHERE type_id = $1`, custom.ID); err == nil {
		t.Error("a version was deleted")
	}
	edited, _, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Prompt: testPrompt + " Tune.", Settings: testTypeSettings()}, testModelDefaults)
	if err != nil || edited.CurrentVersion != 2 || edited.Current.Prompt != testPrompt+" Tune." {
		t.Fatalf("edit: got = %+v, %v, want version 2", edited, err)
	}
	first, err := store.Version(ctx, orgID, custom.ID, 1)
	if err != nil || first.Prompt != testPrompt || first.Digest != custom.Current.Digest {
		t.Errorf("version 1 after the edit: got = %+v, %v, want it as it was", first, err)
	}
	versions, err := store.Versions(ctx, orgID, custom.ID)
	if err != nil || len(versions) != 2 || versions[0].Version != 2 {
		t.Errorf("versions: got = %+v, %v, want 2 then 1", versions, err)
	}
	if again, propagation, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 2, Name: "Tuner 2", Prompt: testPrompt + " Tune.", Settings: testTypeSettings()}, testModelDefaults); err != nil || again.CurrentVersion != 2 || again.Name != "Tuner 2" || propagation.Version != 2 {
		t.Errorf("an edit of the name alone: got = %+v, %+v, %v, want the name changed and no version", again, propagation, err)
	}
}

func TestSeedingAddsAVersionOnlyForADigestNoVersionHas(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	builtin := agenttypes.Builtin{ID: "test-" + uuid.New().String()[:8], Name: "Test", Role: domain.AgentRoleResearcher, Prompt: testPrompt, Settings: testTypeSettings()}
	seed := func(b agenttypes.Builtin) []SeededAgentType {
		t.Helper()
		seeded, err := SeedBuiltinAgentTypes(ctx, pool, []agenttypes.Builtin{b}, testModelDefaults)
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

	seed(builtin)
	if got := current(); got.CurrentVersion != 1 || !got.Builtin || got.OrgID != "" {
		t.Fatalf("first seed: got = %+v, want a built-in at version 1", got)
	}
	if seeded := seed(builtin); len(seeded) != 0 || current().CurrentVersion != 1 {
		t.Errorf("a second seed of the same: seeded %+v, version %d, want nothing", seeded, current().CurrentVersion)
	}
	changed := builtin
	changed.Prompt += " Changed."
	if seeded := seed(changed); len(seeded) != 1 || seeded[0].Propagation.Version != 2 || current().CurrentVersion != 2 {
		t.Errorf("a changed prompt: seeded %+v, version %d, want version 2", seeded, current().CurrentVersion)
	}
	// An older replica of a rolling deploy seeds what it holds: a version already has it.
	if seeded := seed(builtin); len(seeded) != 0 || current().CurrentVersion != 2 {
		t.Errorf("the old prompt again: seeded %+v, version %d, want version 2 kept", seeded, current().CurrentVersion)
	}
	other := changed
	other.Role = domain.AgentRoleBaseline
	if _, err := SeedBuiltinAgentTypes(ctx, pool, []agenttypes.Builtin{other}, testModelDefaults); err == nil {
		t.Error("a built-in seeded with another role was accepted")
	}
}

func TestAnOrgsTypeIsItsAloneAndABuiltinIsNobodys(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	orgID, _, custom := orgWithExperiment(t, pool)
	_, other := newTestUserHome(t, NewAccountsStore(pool))

	if _, err := store.Get(ctx, other, custom.ID); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org reads it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{OrgID: other, ID: custom.ID, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}, testModelDefaults); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org edits it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, err := store.Archive(ctx, other, custom.ID); !errors.Is(err, ErrAgentTypeNotFound) {
		t.Errorf("another org archives it: got = %v, want ErrAgentTypeNotFound", err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: domain.DefaultAgentFlavor, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}, testModelDefaults); !errors.Is(err, ErrAgentTypeBuiltin) {
		t.Errorf("an org edits a built-in: got = %v, want ErrAgentTypeBuiltin", err)
	}
	if _, err := store.Archive(ctx, orgID, domain.DefaultAgentFlavor); !errors.Is(err, ErrAgentTypeBuiltin) {
		t.Errorf("an org archives a built-in: got = %v, want ErrAgentTypeBuiltin", err)
	}
	var conflict *AgentTypeVersionConflict
	if _, _, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 7, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}, testModelDefaults); !errors.As(err, &conflict) || conflict.Current != 1 {
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

func TestATypeInUseIsNotArchivedAndAnArchivedOneTakesNoAgent(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	orgID, peID, custom := orgWithExperiment(t, pool)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET researchers = jsonb_build_array(jsonb_build_object('agent_type', $2::text, 'model', 'm', 'count', 1)) WHERE id = $1`, peID, custom.ID); err != nil {
		t.Fatal(err)
	}
	var inUse *AgentTypeInUse
	if _, err := store.Archive(ctx, orgID, custom.ID); !errors.As(err, &inUse) || !slices.Equal(inUse.PlatformExperimentIDs, []string{peID}) {
		t.Fatalf("archive while a pool names it: got = %v, want in use by %s", err, peID)
	}
	setPEStatus(t, pool, peID, domain.PlatformExpClosed, time.Minute)
	archived, err := store.Archive(ctx, orgID, custom.ID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive once its experiment closed: got = %+v, %v", archived, err)
	}
	if _, _, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "x", Prompt: testPrompt, Settings: testTypeSettings()}, testModelDefaults); !errors.Is(err, ErrAgentTypeArchived) {
		t.Errorf("edit an archived type: got = %v, want ErrAgentTypeArchived", err)
	}
	open := "pe-" + uuid.New().String()[:8]
	createTestPEIn(t, pool, open, defaultProjectOf(t, NewAccountsStore(pool), orgID).ID)
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:8], PerformanceScore: 0.5, CreatedAt: time.Now()}
	if err := pes.SpawnAgent(ctx, open, agent, domain.AgentSignup{AgentID: agent.ID, Role: domain.SignupRoleCompetitor, Flavor: custom.ID, Model: "m"}); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("an agent of an archived type: got = %v, want ErrUnknownAgentType", err)
	}
}

func TestAnAgentIsPinnedToAnAgentTypeOfItsOrgAndItsRole(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	_, peID, custom := orgWithExperiment(t, pool)
	_, _, foreign := orgWithExperiment(t, pool)

	spawn := func(role domain.SignupRole, agentType string) (string, error) {
		agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:8], PerformanceScore: 0.5, CreatedAt: time.Now()}
		return agent.ID, pes.SpawnAgent(ctx, peID, agent, domain.AgentSignup{AgentID: agent.ID, Role: role, Flavor: agentType, Model: "m"})
	}
	own, err := spawn(domain.SignupRoleCompetitor, custom.ID)
	if err != nil {
		t.Fatalf("an agent of its org's type: got = %v, want = nil", err)
	}
	if got := signupOf(t, pes, peID, own); got.Flavor != custom.ID || got.AgentTypeVersion != 1 {
		t.Errorf("its signup: got = %s v%d, want %s v1", got.Flavor, got.AgentTypeVersion, custom.ID)
	}
	if _, err := spawn(domain.SignupRoleCompetitor, foreign.ID); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("another org's type: got = %v, want ErrUnknownAgentType", err)
	}
	if _, err := spawn(domain.SignupRoleBaseline, custom.ID); !errors.Is(err, ErrUnknownAgentType) {
		t.Errorf("a researcher type for a baseline agent: got = %v, want ErrUnknownAgentType", err)
	}
	baseline, err := spawn(domain.SignupRoleBaseline, "")
	if err != nil || signupOf(t, pes, peID, baseline).Flavor != "baseline" {
		t.Errorf("a baseline agent naming no type: got = %v, want the built-in baseline", err)
	}
	src, err := pes.AgentInstructions(ctx, peID, own)
	if err != nil || src == nil || src.Role != domain.AgentRoleResearcher || src.Version.Prompt != testPrompt || src.Model != "m" {
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

func TestAnEditMovesEveryLiveAgentAndTellsEach(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	events := NewEventsStore(pool, noHypothesisEvents{})
	orgID, peID, custom := orgWithExperiment(t, pool)
	ids := spawnTestAgents(t, pes, peID,
		domain.AgentSignup{Flavor: custom.ID, Model: testModelDefaults.Agent},
		domain.AgentSignup{Flavor: custom.ID, Model: "its-own", Hyperparameters: map[string]any{"lr": 1.0}},
		domain.AgentSignup{Flavor: custom.ID, Model: testModelDefaults.Agent})
	follows, own, finished := ids[0], ids[1], ids[2]
	setPEStatus(t, pool, peID, domain.PlatformExpRunning, time.Minute)
	if err := pes.FinishAgent(ctx, peID, finished, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET researchers = jsonb_build_array(jsonb_build_object('agent_type', $2::text, 'model', $3::text, 'count', 3)) WHERE id = $1`, peID, custom.ID, testModelDefaults.Agent); err != nil {
		t.Fatal(err)
	}
	live, stop := listenForEvents(t, pool)
	defer stop()

	settings := testTypeSettings()
	settings.Model, settings.Hyperparameters = "the-new-model", map[string]any{"lr": 0.5}
	edited, propagation, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Prompt: testPrompt, Settings: settings}, testModelDefaults)
	if err != nil || edited.CurrentVersion != 2 {
		t.Fatalf("edit: got = %+v, %v", edited, err)
	}
	moved := map[string]bool{}
	for _, pin := range propagation.Moved {
		moved[pin.AgentID] = true
	}
	if len(propagation.Failed) != 0 || len(moved) != 2 || !moved[follows] || !moved[own] {
		t.Fatalf("propagation: got = %+v, want %s and %s moved", propagation, follows, own)
	}
	if got := signupOf(t, pes, peID, follows); got.AgentTypeVersion != 2 || got.Model != "the-new-model" || got.Hyperparameters["lr"] != 0.5 {
		t.Errorf("an agent on the type's model and hyperparameters: got = v%d %s %v, want v2 on the new ones", got.AgentTypeVersion, got.Model, got.Hyperparameters)
	}
	if got := signupOf(t, pes, peID, own); got.AgentTypeVersion != 2 || got.Model != "its-own" || got.Hyperparameters["lr"] != 1.0 {
		t.Errorf("an agent on its own model and hyperparameters: got = v%d %s %v, want v2 keeping its own", got.AgentTypeVersion, got.Model, got.Hyperparameters)
	}
	if got := signupOf(t, pes, peID, finished); got.AgentTypeVersion != 1 {
		t.Errorf("a finished agent: got = v%d, want v1 kept", got.AgentTypeVersion)
	}
	pe, err := pes.GetPlatformExperiment(ctx, peID)
	if err != nil || len(pe.Researchers) != 1 || pe.Researchers[0].Model != "the-new-model" {
		t.Errorf("the pool: got = %+v, %v, want its entry on the new model", pe.Researchers, err)
	}

	told := map[string]string{}
	deadline := time.After(5 * time.Second)
	for len(told) < 2 {
		select {
		case e := <-live:
			if e.Kind == domain.EventAgentInstructions && e.PlatformExperimentID == peID {
				told[e.AgentID] = e.Value + "/" + e.Detail
			}
		case <-deadline:
			t.Fatalf("told: got = %v, want both live agents", told)
		}
	}
	if told[follows] != "2/"+custom.ID || told[own] != "2/"+custom.ID {
		t.Errorf("told: got = %v, want version 2 of %s to each", told, custom.ID)
	}
	replayed, err := events.Replay(ctx, domain.EventFilter{PlatformExperimentID: peID, AgentID: own}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var instructions []domain.Event
	for _, e := range replayed {
		if e.Kind == domain.EventAgentInstructions {
			instructions = append(instructions, e)
		}
	}
	if len(instructions) != 1 || instructions[0].AgentID != own || instructions[0].Value != "2" {
		t.Errorf("replayed to %s: got = %+v, want its own one", own, instructions)
	}
}

func TestAnAgentThatCannotBeMovedLeavesTheOthersMoved(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	orgID, peID, custom := orgWithExperiment(t, pool)
	ids := spawnTestAgents(t, pes, peID, domain.AgentSignup{Flavor: custom.ID, Model: "m"}, domain.AgentSignup{Flavor: custom.ID, Model: "m"}, domain.AgentSignup{Flavor: custom.ID, Model: "m"})
	broken := ids[1]
	fn := "refuse_" + uuid.New().String()[:8]
	if _, err := pool.pool.Exec(ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'this agent cannot be moved'; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `CREATE TRIGGER `+fn+` BEFORE UPDATE OF agent_type_version ON experiment_signups FOR EACH ROW WHEN (NEW.agent_id = '`+broken+`') EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON experiment_signups`) //nolint:errcheck
		pool.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)                 //nolint:errcheck
	})

	_, propagation, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: "Tuner", Prompt: testPrompt + " Again.", Settings: testTypeSettings()}, testModelDefaults)
	if err != nil {
		t.Fatalf("edit: got = %v, want = nil", err)
	}
	if len(propagation.Moved) != 2 || len(propagation.Failed) != 1 || propagation.Failed[0].AgentID != broken || propagation.Failed[0].Error == "" {
		t.Fatalf("propagation: got = %+v, want two moved and %s failed with why", propagation, broken)
	}
	for _, id := range ids {
		want := 2
		if id == broken {
			want = 1
		}
		if got := signupOf(t, pes, peID, id); got.AgentTypeVersion != want {
			t.Errorf("%s: got = v%d, want v%d", id, got.AgentTypeVersion, want)
		}
	}
}

func TestAnEditOfACoordinatorTypeMovesTheCoordinatorsOfRunsThatGoOn(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewAgentTypesStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	orgID, peID, _ := orgWithExperiment(t, pool)
	coordinatorPrompt := "{{rules}}\n{{hl_commands}}\nCoordinate {{platform_experiment_id}}."
	custom, err := store.Create(ctx, orgID, "Strict coordinator", domain.AgentRoleCoordinator, coordinatorPrompt, testTypeSettings(), domain.Actor{Kind: domain.ActorUser, ID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET coordinator_agent_type = $2, coordinator_agent_type_version = 1, coordinator_model = $3 WHERE id = $1`, peID, custom.ID, testModelDefaults.Coordinator); err != nil {
		t.Fatal(err)
	}
	settings := testTypeSettings()
	settings.Model = "coordinator-next"
	_, propagation, err := store.Edit(ctx, AgentTypeEdit{OrgID: orgID, ID: custom.ID, IfVersion: 1, Name: custom.Name, Prompt: coordinatorPrompt + " Strictly.", Settings: settings}, testModelDefaults)
	if err != nil || len(propagation.Moved) != 1 || propagation.Moved[0] != (domain.PinMove{PlatformExperimentID: peID, AgentID: domain.CoordinatorRecipient}) {
		t.Fatalf("edit: got = %+v, %v, want the coordinator of %s moved", propagation, err, peID)
	}
	src, err := pes.CoordinatorInstructions(ctx, peID)
	if err != nil || src.Version.Version != 2 || src.Model != "coordinator-next" {
		t.Errorf("the coordinator: got = %+v, %v, want version 2 on the type's new model", src, err)
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: peID}, 0)
	if err != nil || !slices.ContainsFunc(replayed, func(e domain.Event) bool {
		return e.Kind == domain.EventAgentInstructions && e.AgentID == domain.CoordinatorRecipient && e.Value == "2"
	}) {
		t.Errorf("replayed: got = %+v, %v, want agent.instructions to the coordinator", replayed, err)
	}
}
