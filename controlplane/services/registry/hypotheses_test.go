package registry

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// poolStore is the shared idea pool reduced to the one rule the database really enforces: a row
// is unique on (platform_experiment_id, normalized_text) and on nothing else. It embeds Store so
// any method a test did not think about panics instead of quietly answering zero.
type poolStore struct {
	Store
	rows map[string]*domain.Hypothesis
}

func newPoolStore() *poolStore {
	return &poolStore{rows: map[string]*domain.Hypothesis{}}
}

// GetPlatformExperimentSubmitPolicies defaults to "not found" — every existing test in this file
// registers hypotheses against a platform experiment id this fake never populates a policy for,
// and RegisterHypothesis must treat that exactly like today's behavior: unrestricted.
func (s *poolStore) GetPlatformExperimentSubmitPolicies(_ context.Context, _ string) (domain.SubmitterPolicy, domain.SubmitterPolicy, bool, error) {
	return "", "", false, nil
}

func (s *poolStore) FindOrCreateHypothesis(_ context.Context, source domain.HypothesisSource, agentID, author, platformExperimentID, text string) (*domain.Hypothesis, bool, error) {
	key := platformExperimentID + "\x00" + domain.NormalizeHypothesisText(text)
	if existing, ok := s.rows[key]; ok {
		return existing, true, nil
	}
	h := &domain.Hypothesis{
		ID: key, AgentID: agentID, Source: source, Author: author,
		PlatformExperimentID: platformExperimentID, Text: text, Status: domain.HypothesisOpen,
	}
	s.rows[key] = h
	return h, false, nil
}

// UpdateHypothesisStatus mirrors the store's single-statement predicate,
// `agent_id = $2 OR agent_id IS NULL`: an owned row answers only to its owner, and a row nobody
// owns answers to whoever tested it.
func (s *poolStore) UpdateHypothesisStatus(_ context.Context, id, callerAgentID string, status domain.HypothesisStatus) (*domain.Hypothesis, error) {
	h, ok := s.rows[id]
	if !ok {
		return nil, nil
	}
	if h.AgentID != "" && h.AgentID != callerAgentID {
		return nil, db.ErrNotOwner
	}
	h.Status = status
	return h, nil
}

// CreateHypothesisComment mirrors the store insert, which consults nothing about the hypothesis
// beyond its id — a note is attached to a row, not authorised against its owner.
func (s *poolStore) CreateHypothesisComment(_ context.Context, hypothesisID string, source domain.HypothesisSource, agentID, author, text string) (*domain.HypothesisComment, error) {
	return &domain.HypothesisComment{
		ID: "c-1", HypothesisID: hypothesisID, Source: source,
		AgentID: agentID, Author: author, Text: text,
	}, nil
}

func poolService(store Store) *Service {
	return &Service{store: store, logger: zap.NewNop()}
}

// A row carrying both an agent_id and a human author would be owned and unowned at the same
// time: quota, standings and the job-ownership check all key on agent_id, so the row would spend
// an agent's budget while presenting as someone's idea. Registration is where this is decided,
// and the only honest answer is to refuse.
func TestRegisterHypothesisRejectsARowClaimingBothAnAgentAndAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "Ada", "pe-1", "warmup helps")
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("RegisterHypothesis with both agent_id and author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}

// A row with neither an agent_id nor an author is attributable to nobody. There is no default to
// fall back on — inventing one would put an unattributed claim in a pool whose entire value is
// that every claim has someone behind it.
func TestRegisterHypothesisRejectsARowWithNeitherAnAgentNorAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, _, err := svc.RegisterHypothesis(context.Background(), "", "", "pe-1", "warmup helps")
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("RegisterHypothesis with neither agent_id nor author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}

// The name a human types is a claim, not an identity — there is no auth — so it must land on the
// row as an author, leaving agent_id empty. Everything that distinguishes a human row later
// (owning no job, holding no quota, appearing in no standings) follows from that empty owner
// column rather than from a branch of its own.
func TestRegisterHypothesisStoresAHumanRowWithAnEmptyAgentIDAndTheTypedName(t *testing.T) {
	svc := poolService(newPoolStore())

	h, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	if h.Source != domain.HypothesisSourceHuman {
		t.Errorf("source: got = %v, want %v", h.Source, domain.HypothesisSourceHuman)
	}
	if h.Author != "Ada" {
		t.Errorf("author: got = %v, want %v", h.Author, "Ada")
	}
	if h.AgentID != "" {
		t.Errorf("agent_id on a human row: got = %v, want %v", h.AgentID, "")
	}
}

// Human ideas are in the same pool under the same dedup, so an agent restating a human's claim
// gets the human's row back rather than a second one. A pool that deduped per source would hold
// the same idea twice and hand every reading agent two entries to reconcile.
func TestRegisteringAnAgentRowWithAHumanRowsTextReturnsTheExistingHumanRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "Warmup   helps.")
	if err != nil {
		t.Fatalf("RegisterHypothesis (human): got = %v, want nil", err)
	}

	agent, existed, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps.")
	if err != nil {
		t.Fatalf("RegisterHypothesis (agent): got = %v, want nil", err)
	}
	if !existed {
		t.Errorf("already_existed: got = %v, want %v", existed, true)
	}
	if agent.ID != human.ID {
		t.Errorf("deduped id: got = %v, want %v", agent.ID, human.ID)
	}
}

// Agents run jobs against human ideas, so a human idea gets settled — and with no owner to settle
// it, the row would sit open forever and the pool would lie about what is still unresolved. The
// rule is therefore the owner predicate read literally: nobody owns it, so whoever tested it may
// record the verdict, with the findings underneath as the record of who earned it.
func TestSetHypothesisStatusLetsAnyAgentSettleAnUnownedHumanRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	settled, err := svc.SetHypothesisStatus(context.Background(), human.ID, "agent-1", domain.HypothesisRefuted)
	if err != nil {
		t.Fatalf("SetHypothesisStatus on a human row: got = %v, want nil", err)
	}
	if settled.Status != domain.HypothesisRefuted {
		t.Errorf("status: got = %v, want %v", settled.Status, domain.HypothesisRefuted)
	}
	if settled.AgentID != "" {
		t.Errorf("agent_id after settling: got = %v, want it to stay unowned", settled.AgentID)
	}
}

// The same rule from the other direction: an agent's own row stays its own to judge, and a
// second agent's verdict on it is refused rather than recorded.
func TestSetHypothesisStatusRefusesAnAgentSettingAnotherAgentsRowStatus(t *testing.T) {
	svc := poolService(newPoolStore())
	owned, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-2", domain.HypothesisRefuted); !errors.Is(err, ErrHypothesisNotOwner) {
		t.Fatalf("SetHypothesisStatus by a non-owner: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-1", domain.HypothesisRefuted); err != nil {
		t.Fatalf("SetHypothesisStatus by the owner: got = %v, want nil", err)
	}
}

// Discussion of an idea has to reach the row the idea is on. If an agent could not comment on a
// human's hypothesis, its observations would have to land somewhere else, and the evidence for one
// claim would end up split across two rows — the same fragmentation that refusing to run jobs
// against human rows caused. Nothing about the hypothesis's source is consulted when a note is
// attached, and this test holds that open.
func TestAddHypothesisCommentAcceptsAnAgentsNoteOnAHumanSubmittedRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps")
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	c, err := svc.AddHypothesisComment(context.Background(), human.ID, "agent-1", "", "retested at 500 steps")
	if err != nil {
		t.Fatalf("AddHypothesisComment on a human row: got = %v, want nil", err)
	}
	if c.AgentID != "agent-1" || c.Source != domain.HypothesisSourceAgent {
		t.Errorf("comment attribution: got = (%v, %v), want (agent-1, %v)", c.AgentID, c.Source, domain.HypothesisSourceAgent)
	}
	if c.HypothesisID != human.ID {
		t.Errorf("comment hypothesis_id: got = %v, want %v", c.HypothesisID, human.ID)
	}
}

// A comment carries the same origin pair as a hypothesis, and for the same reason: a note signed
// by both an agent and a person is signed by neither.
func TestAddHypothesisCommentRejectsARowClaimingBothAnAgentAndAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, err := svc.AddHypothesisComment(context.Background(), "h-1", "agent-1", "Ada", "try a shorter warmup")
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("AddHypothesisComment with both agent_id and author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}
