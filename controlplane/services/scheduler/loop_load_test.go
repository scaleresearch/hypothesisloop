package scheduler

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The admission loop under the load it is built for: clusters of uneven size, several agents
// each keeping a queue, hundreds of jobs of mixed cores, memory and run time that end over
// simulated time. Clusters and rows are in memory; every placement, claim and capacity read is
// the loop's own code.

// simBox is one cluster of one node. cores and memory are what is left to platform jobs, net of
// the cluster-agent and its sidecars, as the runtime reports them.
type simBox struct {
	cores    float64
	memory   int64
	priority int
}

type simRow struct {
	exp    *domain.Experiment
	hours  float64
	queued time.Duration
	placed time.Duration
	// claims counts the claims of this row's current attempt.
	claims int
}

// sim is the store, the clusters, the quota and the metrics store of one simulated deployment.
type sim struct {
	LoopStore
	ClusterCapacity

	t     *testing.T
	mu    sync.Mutex
	now   time.Duration
	boxes map[string]simBox
	stale map[string]bool
	rows  map[string]*simRow
	// budget is each agent's share of the experiment, in accelerator-hours.
	budget map[string]float64
	calls  map[string]int
	locks  map[string]*sync.Mutex
	// overcommit records a claim that left a cluster holding more than it has.
	overcommit []string
	// beforeTick runs before every pass of run.
	beforeTick func()
}

func newSim(t *testing.T, boxes map[string]simBox) *sim {
	return &sim{t: t, boxes: boxes, stale: map[string]bool{}, rows: map[string]*simRow{}, budget: map[string]float64{},
		calls: map[string]int{}, locks: map[string]*sync.Mutex{}}
}

func (s *sim) count(name string) {
	s.mu.Lock()
	s.calls[name]++
	s.mu.Unlock()
}

func (s *sim) submit(exp *domain.Experiment, hours float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp.Status = domain.StatusQueued
	s.rows[exp.ID] = &simRow{exp: exp, hours: hours, queued: s.now}
}

// view is the row as a read returns it: a copy, with its times placed so that the loop, which
// reads the wall clock, sees the ages the simulation has reached.
func (s *sim) view(r *simRow) *domain.Experiment {
	exp := *r.exp
	queued := time.Now().Add(-(s.now - r.queued))
	exp.QueuedAt, exp.CreatedAt = &queued, queued
	return &exp
}

func (s *sim) list(keep func(*simRow) bool) []*domain.Experiment {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]*simRow, 0, len(s.rows))
	for _, r := range s.rows {
		if keep(r) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].queued != rows[j].queued {
			return rows[i].queued < rows[j].queued
		}
		return rows[i].exp.ID < rows[j].exp.ID
	})
	out := make([]*domain.Experiment, len(rows))
	for i, r := range rows {
		out[i] = s.view(r)
	}
	return out
}

func holding(r *simRow) bool {
	return r.exp.Status == domain.StatusSubmitted || r.exp.Status == domain.StatusRunning
}

func (s *sim) ListQueuedExperiments(context.Context) ([]*domain.Experiment, error) {
	s.count("ListQueuedExperiments")
	return s.list(func(r *simRow) bool { return r.exp.Status == domain.StatusQueued }), nil
}
func (s *sim) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	s.count("ListRunningExperiments")
	return s.list(func(r *simRow) bool { return r.exp.Status == domain.StatusRunning }), nil
}
func (s *sim) ListSubmittedExperiments(context.Context) ([]*domain.Experiment, error) {
	s.count("ListSubmittedExperiments")
	return s.list(func(r *simRow) bool { return r.exp.Status == domain.StatusSubmitted }), nil
}
func (s *sim) ListCapacityClaimedExperiments(context.Context) ([]*domain.Experiment, error) {
	s.count("ListCapacityClaimedExperiments")
	return s.list(holding), nil
}

func (s *sim) lock(cluster string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[cluster] == nil {
		s.locks[cluster] = &sync.Mutex{}
	}
	return s.locks[cluster]
}

// ClaimSubmitted is the store's claim: one claim per cluster at a time, the caller's fresh
// capacity check under that lock, and the row placed only while it is still queued.
func (s *sim) ClaimSubmitted(ctx context.Context, id, cluster string, resolved *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.count("ClaimSubmitted")
	lock := s.lock(cluster)
	lock.Lock()
	defer lock.Unlock()
	desired := s.list(func(r *simRow) bool { return r.exp.Status == domain.StatusSubmitted && r.exp.ClusterName == cluster })
	if capacityAvailable != nil {
		if ok, err := capacityAvailable(ctx, desired); err != nil || !ok {
			return false, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	if r == nil || r.exp.Status != domain.StatusQueued {
		return false, nil
	}
	job := *resolved
	r.exp.Status, r.exp.ClusterName, r.exp.ResolvedJob = domain.StatusSubmitted, cluster, &job
	r.exp.NotAdmittedReason, r.exp.NotAdmittedMessage = "", ""
	r.placed = s.now
	r.claims++
	if free := s.freeLocked(cluster); negative(free) {
		s.overcommit = append(s.overcommit, fmt.Sprintf("%s on %s left %s", id, cluster, footprintStr(free)))
	}
	return true, nil
}

func negative(fp domain.Footprint) bool {
	for _, v := range fp {
		if v < 0 {
			return true
		}
	}
	return false
}

func (s *sim) MarkQueued(_ context.Context, id, reason string) error {
	s.count("MarkQueued")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil && r.exp.Status == domain.StatusSubmitted {
		s.requeueLocked(r, reason)
	}
	return nil
}

func (s *sim) requeueLocked(r *simRow, reason string) {
	r.exp.Status, r.exp.ClusterName, r.exp.ResolvedJob, r.claims = domain.StatusQueued, "", nil, 0
	r.exp.NotAdmittedReason, r.exp.NotAdmittedMessage = domain.SplitQueueReason(reason)
}

func (s *sim) RequeuePreempted(_ context.Context, id string, remaining, cost float64) (bool, error) {
	s.count("RequeuePreempted")
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	if r == nil || r.exp.Status != domain.StatusRunning {
		return false, nil
	}
	r.hours -= (s.now - r.placed).Hours()
	r.exp.EstimatedDurationHours, r.exp.EstimatedCostAccH = remaining, cost
	s.requeueLocked(r, domain.QueueReasonAwaitingAdmission)
	return true, nil
}

func (s *sim) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	s.count("UpdateNotAdmittedReason")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil && r.exp.Status == domain.StatusQueued {
		r.exp.NotAdmittedReason, r.exp.NotAdmittedMessage = domain.SplitQueueReason(reason)
	}
	return nil
}

func (s *sim) MarkHeldJobs(context.Context, time.Duration) error {
	s.count("MarkHeldJobs")
	return nil
}
func (s *sim) IsAgentCut(context.Context, string, string) (bool, error) {
	s.count("IsAgentCut")
	return false, nil
}
func (s *sim) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	s.count("GetPlatformExperiment")
	return &domain.PlatformExperiment{ID: id}, nil
}
func (s *sim) GetClusterSettings(context.Context, string) (*domain.ClusterSettings, error) {
	return nil, nil
}
func (s *sim) RecentlyTriedClusters(context.Context, time.Duration) (map[string]bool, error) {
	return nil, nil
}
func (s *sim) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	s.count("RegisteredClusters")
	out := map[string]domain.Cluster{}
	for name, box := range s.boxes {
		out[name] = domain.Cluster{Name: name, Priority: box.priority}
	}
	return out, nil
}

// The quota: an agent spends its share on every job it has placed, and admission refuses a job
// that would take it over.

type simQuotaRefused struct{}

func (simQuotaRefused) Error() string           { return "insufficient quota" }
func (simQuotaRefused) InsufficientQuota() bool { return true }

func (s *sim) spentLocked(agent, except string) float64 {
	spent := 0.0
	for id, r := range s.rows {
		if id != except && r.exp.AgentID == agent && r.exp.Status != domain.StatusQueued {
			spent += r.exp.EstimatedCostAccH
		}
	}
	return spent
}

func (s *sim) GetAgentQuota(_ context.Context, agent, _ string) (*domain.AgentQuota, error) {
	s.count("GetAgentQuota")
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := s.budget[agent]
	return &domain.AgentQuota{AgentID: agent, GuaranteedAcceleratorHours: budget, BurstAcceleratorHours: budget, UsedGuaranteedAccH: s.spentLocked(agent, "")}, nil
}

func (s *sim) ReserveAdmittedFlavor(_ context.Context, id string, _ domain.AcceleratorType, cost float64, _ int) error {
	s.count("ReserveAdmittedFlavor")
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.rows[id].exp.AgentID
	if s.spentLocked(agent, id)+cost > s.budget[agent] {
		return simQuotaRefused{}
	}
	return nil
}

// The metrics store.

func (s *sim) ObservedElapsedHours(_ context.Context, id string, _, _ time.Time) (float64, error) {
	s.count("ObservedElapsedHours")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil && holding(r) {
		return (s.now - r.placed).Hours(), nil
	}
	return 0, nil
}
func (s *sim) ObservedStintElapsedHours(ctx context.Context, id string, createdAt, now time.Time) (float64, error) {
	return s.ObservedElapsedHours(ctx, id, createdAt, now)
}
func (s *sim) LatestExperimentNode(_ context.Context, id string, _, _ time.Time) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil && r.exp.ClusterName != "" {
		return "node-" + r.exp.ClusterName, true, nil
	}
	return "", false, nil
}
func (s *sim) RecordAdmission(context.Context, string, time.Time) error { return nil }

// The clusters: what each has free is what it has less every placed job's footprint, as
// ClusterReports derives it from desired state.

func (b simBox) total() domain.Footprint {
	return domain.CapacityFootprint(b.cores, map[string]int64{string(cpuType): int64(b.cores)}, b.memory, 1<<40)
}

func (s *sim) freeLocked(cluster string) domain.Footprint {
	return s.freeOf(cluster, holding)
}

// observedLocked is what the runtime reports free: a job placed this pass has no pod yet.
func (s *sim) observedLocked(cluster string) domain.Footprint {
	return s.freeOf(cluster, func(r *simRow) bool { return r.exp.Status == domain.StatusRunning })
}

func (s *sim) freeOf(cluster string, holds func(*simRow) bool) domain.Footprint {
	free := s.boxes[cluster].total()
	for _, r := range s.rows {
		if holds(r) && r.exp.ClusterName == cluster {
			free = free.Sub(r.exp.Footprint())
		}
	}
	return free
}

func (s *sim) live() []string {
	var out []string
	for name := range s.boxes {
		if !s.stale[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *sim) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	s.count("GetFlavorCapacity")
	s.mu.Lock()
	defer s.mu.Unlock()
	available, desiredFree := map[string]domain.Footprint{}, map[string]domain.Footprint{}
	for _, name := range s.live() {
		available[name], desiredFree[name] = s.freeLocked(name), s.freeLocked(name)
	}
	return available, desiredFree, nil
}

func node(fp domain.Footprint) map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: fp[domain.ResourceKey{Kind: domain.ResourceKindCPU}],
		domain.NodeResourceMemoryBytes:   fp[domain.ResourceKey{Kind: domain.ResourceKindMemory}],
		domain.NodeResourceStorageBytes:  fp[domain.ResourceKey{Kind: domain.ResourceKindStorage}],
	}
}

func (s *sim) perNode(value func(string) map[string]int64) map[string]map[string]map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]map[string]int64{}
	for _, name := range s.live() {
		out[name] = map[string]map[string]int64{"node-" + name: value(name)}
	}
	return out
}

func (s *sim) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return s.perNode(func(c string) map[string]int64 {
		return map[string]int64{string(cpuType): s.observedLocked(c)[domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: string(cpuType)}]}
	}), nil
}
func (s *sim) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return s.perNode(func(c string) map[string]int64 { return node(s.observedLocked(c)) }), nil
}
func (s *sim) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return s.perNode(func(c string) map[string]int64 { return node(s.boxes[c].total()) }), nil
}
func (s *sim) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	out := map[string]map[string]map[string]string{}
	for name := range s.perNode(func(string) map[string]int64 { return nil }) {
		out[name] = map[string]map[string]string{"node-" + name: {}}
	}
	return out, nil
}
func (s *sim) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]domain.Footprint{}
	for _, name := range s.live() {
		out[name] = s.boxes[name].total()
	}
	return out, nil
}
func (s *sim) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// advance moves the clock by d: placed jobs start, and those whose run time is up complete. It
// returns how many completed.
func (s *sim) advance(d time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now += d
	ended := 0
	for _, r := range s.rows {
		if r.exp.Status == domain.StatusSubmitted {
			r.exp.Status = domain.StatusRunning
		}
		if r.exp.Status == domain.StatusRunning && s.now-r.placed >= time.Duration(r.hours*float64(time.Hour)) {
			r.exp.Status = domain.StatusCompleted
			ended++
		}
	}
	return ended
}

// nextEnd is when the next placed job completes, or false with none placed.
func (s *sim) nextEnd() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, any := time.Duration(math.MaxInt64), false
	for _, r := range s.rows {
		if holding(r) {
			next, any = min(next, r.placed+time.Duration(r.hours*float64(time.Hour))), true
		}
	}
	return next, any
}

func (s *sim) loop() *Loop {
	l := NewLoop(s, s, s, testGraceLimits, zap.NewNop())
	l.observed = s
	l.evictor = &recordingEvictor{}
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}
	l.settler = &recordingSettler{}
	l.guaranteedFairnessWindow = time.Minute
	l.heldJobNotice = 5 * time.Minute
	return l
}

// Three clusters of uneven size: a small one, a middle one and a large one.
func unevenBoxes() map[string]simBox {
	return map[string]simBox{
		"box-a": {cores: 11.6, memory: 30 << 30, priority: domain.DefaultClusterPriority},
		"box-b": {cores: 27.8, memory: 62 << 30, priority: domain.DefaultClusterPriority},
		"box-c": {cores: 47.5, memory: 120 << 30, priority: domain.DefaultClusterPriority},
	}
}

// simJob is a job on cores with memory per core within every box's share, of an estimate that
// the run takes most of.
func simJob(rng *rand.Rand, id, agent string, tier domain.CapacityTier, cores int, hours float64) *domain.Experiment {
	exp := coresJob(id, tier, cores)
	exp.AgentID = agent
	exp.Job.Memory = fmt.Sprintf("%dMi", cores*[]int{512, 1024, 2048}[rng.IntN(3)])
	exp.Job.Storage = fmt.Sprintf("%dGi", 1+rng.IntN(20))
	return priced(exp, hours)
}

// smallCores and runHours are the sizes and lengths of the researchers' jobs: mostly a few
// cores, mostly hours, some a day.
func smallCores(rng *rand.Rand) int { return []int{1, 1, 2, 2, 2, 4, 4, 6}[rng.IntN(8)] }
func runHours(rng *rand.Rand) float64 {
	return []float64{0.25, 0.5, 1, 2, 3, 6, 12, 24, 30}[rng.IntN(9)]
}

// tickRecord is what one admission pass did.
type tickRecord struct {
	took     time.Duration
	queued   int
	admitted []*domain.Experiment
	calls    map[string]int
}

func (s *sim) tick(l *Loop) tickRecord {
	s.t.Helper()
	if s.beforeTick != nil {
		s.beforeTick()
	}
	s.mu.Lock()
	before := map[string]bool{}
	for id, r := range s.rows {
		if r.exp.Status == domain.StatusQueued {
			before[id] = true
		}
	}
	s.calls = map[string]int{}
	s.mu.Unlock()
	start := time.Now()
	if err := l.tick(context.Background()); err != nil {
		s.t.Fatalf("tick at %v: %v", s.now, err)
	}
	rec := tickRecord{took: time.Since(start), queued: len(before)}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range before {
		if r := s.rows[id]; r.exp.Status != domain.StatusQueued {
			rec.admitted = append(rec.admitted, s.view(r))
		}
	}
	rec.calls = s.calls
	return rec
}

// run ticks on every completion and at every heartbeat until the clock reaches end, calling each
// after every pass.
func (s *sim) run(l *Loop, heartbeat, end time.Duration, each func(tickRecord)) {
	s.t.Helper()
	each(s.tick(l))
	for s.now < end {
		step := heartbeat
		if next, ok := s.nextEnd(); ok && next-s.now < step {
			step = max(next-s.now, time.Second)
		}
		s.advance(step)
		each(s.tick(l))
	}
}

var clusterNamed = regexp.MustCompile(`cluster (\S+) `)

// heldLocked is the clusters held for a job that has waited long enough to hold one (holdRoom):
// the cluster its reason names, every cluster when its reason names none.
func (s *sim) heldLocked(l *Loop) map[string]bool {
	held := map[string]bool{}
	for _, r := range s.rows {
		waited := s.now - r.queued
		if r.exp.Status != domain.StatusQueued || waited < l.heldJobNotice || waited.Hours() < r.exp.EstimatedDurationHours {
			continue
		}
		switch m := clusterNamed.FindStringSubmatch(r.exp.NotAdmittedMessage); {
		case r.exp.NotAdmittedReason != domain.NotAdmittedCapacityUnavailable && r.exp.NotAdmittedReason != domain.NotAdmittedOutranked:
		case m != nil:
			held[m[1]] = true
		default:
			for name := range s.boxes {
				held[name] = true
			}
		}
	}
	return held
}

func (s *sim) allowedLocked(exp *domain.Experiment) bool {
	return s.spentLocked(exp.AgentID, exp.ID)+exp.EstimatedCostAccH <= s.budget[exp.AgentID]
}

// idleRoom names each live cluster not held for a waiting job that has room for a queued job its
// quota allows, while that job waits.
func (s *sim) idleRoom(l *Loop) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.heldLocked(l)
	var idle []string
	for _, name := range s.live() {
		free := s.freeLocked(name)
		for _, r := range s.rows {
			if r.exp.Status == domain.StatusQueued && !held[name] && s.allowedLocked(r.exp) && domain.Fits(free, r.exp.Footprint()) {
				idle = append(idle, fmt.Sprintf("%s has %s free and %s waits (%s: %s)", name, footprintStr(free), r.exp.ID, r.exp.NotAdmittedReason, r.exp.NotAdmittedMessage))
			}
		}
	}
	sort.Strings(idle)
	return idle
}

// usedCores is the part of the live clusters' cores that placed jobs hold.
func (s *sim) usedCores() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var used, total float64
	for _, name := range s.live() {
		total += math.Floor(s.boxes[name].cores)
		for _, r := range s.rows {
			if holding(r) && r.exp.ClusterName == name {
				used += float64(r.exp.AcceleratorCount)
			}
		}
	}
	return used / total
}

func joinFirst(lines []string, n int) string {
	if len(lines) > n {
		lines = append(lines[:n:n], fmt.Sprintf("... %d more", len(lines)-n))
	}
	return strings.Join(lines, "\n  ")
}

// researchers keeps each agent's jobs coming: an agent keeps outstanding jobs queued or placed,
// submitting one as one ends.
type researchers struct {
	rng         *rand.Rand
	outstanding map[string]int
	size        func(*rand.Rand) int
	tier        domain.CapacityTier
	next        int
}

func (rs *researchers) topUp(s *sim) {
	s.mu.Lock()
	open := map[string]int{}
	for _, r := range s.rows {
		if r.exp.Status == domain.StatusQueued || holding(r) {
			open[r.exp.AgentID]++
		}
	}
	s.mu.Unlock()
	agents := slices.Sorted(maps.Keys(rs.outstanding))
	for _, agent := range agents {
		for ; open[agent] < rs.outstanding[agent]; open[agent]++ {
			rs.next++
			s.submitJob(rs.rng, fmt.Sprintf("%s-%d", agent, rs.next), agent, rs.tier, rs.size(rs.rng), runHours(rs.rng))
		}
	}
}

// submitJob queues a job whose run takes most of its estimate.
func (s *sim) submitJob(rng *rand.Rand, id, agent string, tier domain.CapacityTier, cores int, estimate float64) {
	s.submit(simJob(rng, id, agent, tier, cores, estimate), estimate*0.9)
}

// loadedDeployment is three clusters of uneven size and five agents: one has queued a backlog of
// 300 jobs at once, four keep eight jobs each queued or placed. light-d's share of the budget
// runs out.
func loadedDeployment(t *testing.T) (*sim, *researchers) {
	withCPUType(t)
	s := newSim(t, unevenBoxes())
	rs := &researchers{rng: rand.New(rand.NewPCG(7, 11)), outstanding: map[string]int{}, size: smallCores, tier: domain.CapacityGuaranteed}
	for _, agent := range []string{"light-a", "light-b", "light-c", "light-d"} {
		rs.outstanding[agent] = 8
		s.budget[agent] = 1e6
	}
	s.budget["light-d"] = 150
	s.budget["deep"] = 1e6
	for i := range 300 {
		s.submitJob(rs.rng, fmt.Sprintf("deep-%d", i), "deep", domain.CapacityGuaranteed, smallCores(rs.rng), runHours(rs.rng))
	}
	rs.topUp(s)
	return s, rs
}

// Every pass, by a completion or the heartbeat, leaves no cluster with room for a waiting job
// that fits it, unless the room is held for a job that has waited long. An agent with a deep
// backlog gets its turn beside the others and no more, and no agent spends past its share.
func TestUnderLoadEveryClusterStaysBusyAndEveryAgentGetsItsTurn(t *testing.T) {
	s, rs := loadedDeployment(t)
	l := s.loop()
	var idle []string
	ticks, used := 0, 0.0
	s.run(l, 10*time.Minute, 72*time.Hour, func(rec tickRecord) {
		ticks++
		used += s.usedCores()
		if v := s.idleRoom(l); len(v) > 0 && len(idle) < 5 {
			idle = append(idle, fmt.Sprintf("at %v: %s", s.now, joinFirst(v, 3)))
		}
		s.mu.Lock()
		for agent, budget := range s.budget {
			if spent := s.spentLocked(agent, ""); spent > budget {
				t.Errorf("at %v %s spent %.1f of a share of %.1f", s.now, agent, spent, budget)
			}
		}
		s.mu.Unlock()
		rs.topUp(s)
	})
	if len(idle) > 0 {
		t.Errorf("room left idle while a job that fits it waits:\n%s", strings.Join(idle, "\n"))
	}
	if len(s.overcommit) > 0 {
		t.Errorf("claims overcommitted a cluster: %v", s.overcommit)
	}
	if mean := used / float64(ticks); mean < 0.95 {
		t.Errorf("mean share of cores held over %d passes = %.3f, want at least 0.95", ticks, mean)
	} else {
		t.Logf("mean share of cores held over %d passes: %.3f", ticks, mean)
	}

	coreHours := map[string]float64{}
	s.mu.Lock()
	for _, r := range s.rows {
		if r.exp.Status != domain.StatusQueued && r.placed < 48*time.Hour {
			coreHours[r.exp.AgentID] += float64(r.exp.AcceleratorCount) * r.hours
		}
	}
	if spent := s.spentLocked("light-d", ""); spent < 0.8*s.budget["light-d"] {
		t.Errorf("light-d spent %.1f of its share of %.1f: the run never reached it", spent, s.budget["light-d"])
	}
	s.mu.Unlock()
	t.Logf("core-hours placed in the first 48h: %v", coreHours)
	for _, agent := range []string{"light-a", "light-b", "light-c"} {
		if coreHours[agent] < 0.25*coreHours["deep"] {
			t.Errorf("%s placed %.0f core-hours while the agent with the backlog placed %.0f", agent, coreHours[agent], coreHours["deep"])
		}
	}
}

// A stream of small jobs keeps every cluster full. Jobs of 8 to 40 cores, some of which fit only
// the largest cluster, each start once they have waited long enough to hold a cluster's room;
// without the hold, the small jobs take every core as it frees and most never do.
func TestALargeJobIsNotStarvedByAStreamOfSmallOnes(t *testing.T) {
	withCPUType(t)
	run := func(hold bool) map[string]*simRow {
		s := newSim(t, unevenBoxes())
		rs := &researchers{rng: rand.New(rand.NewPCG(3, 5)), outstanding: map[string]int{}, tier: domain.CapacityGuaranteed,
			size: func(rng *rand.Rand) int { return []int{1, 1, 2, 2, 3}[rng.IntN(5)] }}
		for _, agent := range []string{"light-a", "light-b", "light-c", "light-d", "light-e"} {
			rs.outstanding[agent] = 14
			s.budget[agent] = 1e6
		}
		s.budget["big"] = 1e6
		rs.topUp(s)
		l := s.loop()
		if !hold {
			l.heldJobNotice = 0
		}
		big := map[string]*simRow{}
		s.run(l, 10*time.Minute, 120*time.Hour, func(tickRecord) {
			if s.now >= time.Hour && len(big) == 0 {
				for i, cores := range []int{8, 11, 16, 24, 40} {
					id := fmt.Sprintf("big-%d", i)
					s.submitJob(rs.rng, id, "big", domain.CapacityGuaranteed, cores, 6)
					big[id] = s.rows[id]
				}
			}
			rs.topUp(s)
		})
		if len(s.overcommit) > 0 {
			t.Errorf("claims overcommitted a cluster: %v", s.overcommit)
		}
		return big
	}

	for id, r := range run(true) {
		if r.exp.Status == domain.StatusQueued {
			t.Errorf("%s of %d cores still waits after %v", id, r.exp.AcceleratorCount, 119*time.Hour)
		} else if waited := r.placed - r.queued; waited > 60*time.Hour {
			t.Errorf("%s of %d cores waited %v", id, r.exp.AcceleratorCount, waited)
		} else {
			t.Logf("%s of %d cores waited %v and ran on %s", id, r.exp.AcceleratorCount, waited, r.exp.ClusterName)
		}
	}
	starved := 0
	for _, r := range run(false) {
		if r.exp.Status == domain.StatusQueued {
			starved++
		}
	}
	if starved < 2 {
		t.Errorf("without the hold %d of the large jobs never started; the stream of small jobs does not test starvation", starved)
	}
}

// overflowBoxes is two clusters of the org's and a shared one ranked below them.
func overflowBoxes() map[string]simBox {
	boxes := unevenBoxes()
	delete(boxes, "box-c")
	boxes["shared-pool"] = simBox{cores: 23.8, memory: 60 << 30, priority: domain.SharedClusterPriority}
	return boxes
}

// A guaranteed job that fits is placed before a burst job takes the room, and a job goes to a
// cluster of a lower priority only when none above has room for it.
func TestUnderLoadPriorityComesFirst(t *testing.T) {
	withCPUType(t)
	s := newSim(t, overflowBoxes())
	rs := &researchers{rng: rand.New(rand.NewPCG(13, 17)), outstanding: map[string]int{}, size: smallCores, tier: domain.CapacityGuaranteed}
	for _, agent := range []string{"light-a", "light-b", "light-c"} {
		rs.outstanding[agent] = 10
		s.budget[agent] = 1e6
	}
	s.budget["deep"] = 1e6
	for i := range 200 {
		s.submitJob(rs.rng, fmt.Sprintf("deep-%d", i), "deep", domain.CapacityBurst, smallCores(rs.rng), runHours(rs.rng))
	}
	rs.topUp(s)
	l := s.loop()
	var wrong []string
	burstPlaced, lowerPlaced := 0, 0
	s.run(l, 10*time.Minute, 48*time.Hour, func(rec tickRecord) {
		s.mu.Lock()
		held := s.heldLocked(l)
		burstOn := map[string]domain.Footprint{}
		for _, exp := range rec.admitted {
			if exp.CapacityTier == domain.CapacityBurst {
				burstPlaced++
				if burstOn[exp.ClusterName] == nil {
					burstOn[exp.ClusterName] = domain.NewFootprint()
				}
				burstOn[exp.ClusterName].AddFootprint(exp.Footprint())
			}
			if s.boxes[exp.ClusterName].priority == domain.SharedClusterPriority {
				lowerPlaced++
				for name, box := range s.boxes {
					if box.priority > domain.SharedClusterPriority && !held[name] && domain.Fits(s.freeLocked(name), exp.Footprint()) {
						wrong = append(wrong, fmt.Sprintf("at %v %s went to shared-pool while %s had room", s.now, exp.ID, name))
					}
				}
			}
		}
		for cluster, taken := range burstOn {
			room := s.freeLocked(cluster).Sub(domain.NewFootprint())
			room.AddFootprint(taken)
			for _, r := range s.rows {
				if r.exp.Status == domain.StatusQueued && r.exp.CapacityTier == domain.CapacityGuaranteed && !held[cluster] && domain.Fits(room, r.exp.Footprint()) {
					wrong = append(wrong, fmt.Sprintf("at %v burst jobs took room on %s that guaranteed %s fits", s.now, cluster, r.exp.ID))
				}
			}
		}
		s.mu.Unlock()
		rs.topUp(s)
	})
	if len(wrong) > 0 {
		t.Errorf("%d placements out of order:\n%s", len(wrong), joinFirst(wrong, 5))
	}
	if burstPlaced == 0 || lowerPlaced == 0 {
		t.Errorf("placed %d burst jobs and %d on the shared cluster: the run did not test the order", burstPlaced, lowerPlaced)
	}
	t.Logf("placed %d burst jobs, %d on the lower cluster", burstPlaced, lowerPlaced)
}

// A pass over 600 queued jobs reads each agent's state once, not each job's, and a pass that
// changes nothing writes nothing.
func TestAPassOverHundredsOfQueuedJobsReadsPerAgentNotPerJob(t *testing.T) {
	withCPUType(t)
	boxes := unevenBoxes()
	boxes["tt-gpu-host"] = simBox{cores: 63.5, memory: 250 << 30, priority: domain.DefaultClusterPriority}
	s := newSim(t, boxes)
	rng := rand.New(rand.NewPCG(1, 2))
	agents := []string{"a1", "a2", "a3", "a4", "a5", "a6"}
	for _, agent := range agents {
		s.budget[agent] = 1e6
	}
	for i := range 600 {
		s.submitJob(rng, fmt.Sprintf("job-%d", i), agents[i%len(agents)], domain.CapacityGuaranteed, smallCores(rng), runHours(rng))
	}
	l := s.loop()

	first := s.tick(l)
	s.advance(time.Minute)
	s.tick(l)
	s.advance(time.Minute)
	steady := s.tick(l)
	t.Logf("first pass over %d queued: %v, admitted %d, calls %v", first.queued, first.took, len(first.admitted), first.calls)
	t.Logf("next pass over %d queued: %v, calls %v", steady.queued, steady.took, steady.calls)

	for _, read := range []string{"IsAgentCut", "GetAgentQuota", "GetPlatformExperiment", "ObservedElapsedHours"} {
		if n := first.calls[read]; n > len(agents) {
			t.Errorf("%s was read %d times in a pass over %d queued jobs of %d agents", read, n, first.queued, len(agents))
		}
	}
	if n := first.calls["ClaimSubmitted"]; n != len(first.admitted) {
		t.Errorf("%d claims for %d admitted jobs", n, len(first.admitted))
	}
	if n := steady.calls["UpdateNotAdmittedReason"]; n != 0 {
		t.Errorf("a pass that changed nothing rewrote %d queue reasons", n)
	}
	for _, rec := range []tickRecord{first, steady} {
		if rec.took > time.Second {
			t.Errorf("a pass over %d queued jobs took %v", rec.queued, rec.took)
		}
	}
}

// The priority pass after admission reads each agent's quota once and writes only scores that
// changed.
func TestReprioritizingAQueueReadsEachAgentsQuotaOnce(t *testing.T) {
	withCPUType(t)
	rng := rand.New(rand.NewPCG(1, 2))
	store := &priorityStore{}
	for i := range 600 {
		exp := simJob(rng, fmt.Sprintf("job-%d", i), fmt.Sprintf("agent-%d", i%6), domain.CapacityGuaranteed, smallCores(rng), runHours(rng))
		exp.HypothesisID = fmt.Sprintf("h-%d", i%40)
		store.queued = append(store.queued, exp)
	}
	quota := &countingQuota{}
	svc := &Service{store: store, quota: quota, weights: domain.SchedulingWeights{W1Novelty: 1, W3CostEfficiency: 1}, logger: zap.NewNop()}

	if err := svc.RePrioritize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if quota.reads > 6 || store.writes != 600 {
		t.Errorf("first pass read quota %d times and wrote %d scores, want at most 6 reads and 600 writes", quota.reads, store.writes)
	}
	store.writes = 0
	if err := svc.RePrioritize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.writes != 0 {
		t.Errorf("a pass with nothing changed wrote %d scores", store.writes)
	}
}

type priorityStore struct {
	Store
	queued []*domain.Experiment
	writes int
}

func (s *priorityStore) ListExperiments(context.Context, domain.ExperimentFilter) ([]*domain.Experiment, error) {
	return s.queued, nil
}
func (s *priorityStore) GetRunningAndQueued(context.Context, string) ([]*domain.Experiment, error) {
	return s.queued, nil
}
func (s *priorityStore) UpdateExperimentPriority(_ context.Context, id string, score float64) error {
	s.writes++
	for _, exp := range s.queued {
		if exp.ID == id {
			exp.PriorityScore = score
		}
	}
	return nil
}

type countingQuota struct {
	QuotaService
	reads int
}

func (q *countingQuota) GetAgentQuota(_ context.Context, agent, _ string) (*domain.AgentQuota, error) {
	q.reads++
	return &domain.AgentQuota{AgentID: agent, GuaranteedAcceleratorHours: 100}, nil
}

type countingNovelty struct{}

func (countingNovelty) ComputeNovelty(_ context.Context, hypothesisID string, existing []*domain.Experiment) (float64, error) {
	n := 0.0
	for _, exp := range existing {
		if exp.HypothesisID == hypothesisID {
			n++
		}
	}
	return 1 / (1 + n), nil
}

// Two admission passes running at once, as two replicas would through a failover, beside jobs
// being submitted and ending: no cluster is ever claimed past what it has, and no job is placed
// twice.
func TestPassesAtOnceWithSubmitsAndCompletionsNeverOvercommit(t *testing.T) {
	withCPUType(t)
	s := newSim(t, unevenBoxes())
	rng := rand.New(rand.NewPCG(5, 8))
	for _, agent := range []string{"a1", "a2", "a3", "a4"} {
		s.budget[agent] = 1e6
	}
	for i := range 300 {
		s.submitJob(rng, fmt.Sprintf("job-%d", i), fmt.Sprintf("a%d", 1+i%4), domain.CapacityGuaranteed, smallCores(rng), runHours(rng))
	}
	loops := []*Loop{s.loop(), s.loop()}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := l.tick(context.Background()); err != nil {
					t.Errorf("tick: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		submitter := rand.New(rand.NewPCG(9, 9))
		for i := range 400 {
			s.submitJob(submitter, fmt.Sprintf("late-%d", i), fmt.Sprintf("a%d", 1+i%4), domain.CapacityGuaranteed, smallCores(submitter), runHours(submitter))
			s.advance(10 * time.Minute)
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()
	wg.Wait()

	if len(s.overcommit) > 0 {
		t.Fatalf("claims overcommitted a cluster: %s", joinFirst(s.overcommit, 5))
	}
	placed := 0
	for id, r := range s.rows {
		if r.claims > 1 {
			t.Errorf("%s was claimed %d times in one attempt", id, r.claims)
		}
		if r.exp.Status != domain.StatusQueued {
			placed++
		}
	}
	if placed < 100 {
		t.Errorf("placed %d jobs: the passes did not run against each other", placed)
	}
	t.Logf("placed %d of %d jobs across two passes running at once", placed, len(s.rows))
}

// A cluster whose agent stops reporting gets no new job, the jobs that would have gone there go
// to the others, and it takes jobs again once it reports.
func TestAClusterThatStopsReportingGetsNoJobsAndTheQueueMovesOn(t *testing.T) {
	s, rs := loadedDeployment(t)
	l := s.loop()
	var wrong, idle []string
	placedOn := map[string]int{}
	quiet := func() bool { return s.now >= 6*time.Hour && s.now < 18*time.Hour }
	s.beforeTick = func() {
		s.mu.Lock()
		s.stale["box-b"] = quiet()
		s.mu.Unlock()
	}
	s.run(l, 10*time.Minute, 36*time.Hour, func(rec tickRecord) {
		for _, exp := range rec.admitted {
			if s.stale[exp.ClusterName] {
				wrong = append(wrong, fmt.Sprintf("at %v %s placed on %s", s.now, exp.ID, exp.ClusterName))
			}
			if quiet() {
				placedOn[exp.ClusterName]++
			}
		}
		if v := s.idleRoom(l); len(v) > 0 && len(idle) < 5 {
			idle = append(idle, fmt.Sprintf("at %v: %s", s.now, joinFirst(v, 3)))
		}
		rs.topUp(s)
	})
	if len(wrong) > 0 {
		t.Errorf("jobs placed on a cluster that stopped reporting:\n%s", joinFirst(wrong, 5))
	}
	if len(idle) > 0 {
		t.Errorf("room left idle while a job that fits it waits:\n%s", strings.Join(idle, "\n"))
	}
	if placedOn["box-a"] == 0 || placedOn["box-c"] == 0 {
		t.Errorf("while box-b was quiet the others took %v", placedOn)
	}
	s.mu.Lock()
	back := 0
	for _, r := range s.rows {
		if r.exp.ClusterName == "box-b" && r.placed >= 18*time.Hour {
			back++
		}
	}
	s.mu.Unlock()
	if back == 0 {
		t.Error("box-b took no job after it reported again")
	}
}

// statusSim is the job watcher's view of the simulation.
type statusSim struct{ *sim }

func (s statusSim) ListExperimentsWithStatus(context.Context, domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return nil, nil
}
func (s statusSim) MarkStarted(context.Context, string) (bool, error) { return false, nil }
func (s statusSim) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(s.rows[id]), nil
}
func (s statusSim) TransitionStatus(_ context.Context, id string, from, to domain.ExperimentStatus, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[id]; r != nil && r.exp.Status == from {
		r.exp.Status = to
		return true, nil
	}
	return false, nil
}
func (s statusSim) ResolveTermination(context.Context, string, domain.ExperimentStatus, domain.ExperimentStatus, string) (domain.Termination, error) {
	return domain.TerminationSkipped, nil
}
func (s statusSim) RequeueForRetry(context.Context, string, int) (bool, error) { return false, nil }
func (s statusSim) MarkQuotaSettled(context.Context, string) error             { return nil }

// A job's end wakes admission at once: the job waiting for its room is placed in well under the
// heartbeat, which here is an hour.
func TestAJobsEndWakesAdmissionAndItsRoomIsRefilled(t *testing.T) {
	withCPUType(t)
	s := newSim(t, map[string]simBox{"box-a": {cores: 11.6, memory: 30 << 30, priority: domain.DefaultClusterPriority}})
	s.budget["a1"] = 1e6
	rng := rand.New(rand.NewPCG(1, 1))
	s.submitJob(rng, "running", "a1", domain.CapacityGuaranteed, 11, 24)
	l := s.loop()
	s.tick(l)
	s.advance(time.Minute)
	s.submitJob(rng, "waiting", "a1", domain.CapacityGuaranteed, 8, 24)
	s.tick(l)

	l.heartbeat = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Start(ctx, time.Minute)
	w := NewJobWatcher(statusSim{s}, nil, noopSettler{}, zap.NewNop()).WithCapacityFreed(l.Trigger)

	ended := time.Now()
	running, _ := statusSim{s}.GetExperiment(ctx, "running")
	if err := w.onFinished(ctx, running, domain.StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	for {
		s.mu.Lock()
		status := s.rows["waiting"].exp.Status
		s.mu.Unlock()
		if status == domain.StatusSubmitted {
			t.Logf("the waiting job was placed %v after the running one ended", time.Since(ended))
			return
		}
		if time.Since(ended) > 5*time.Second {
			t.Fatalf("the waiting job is still %s %v after the running one ended", status, time.Since(ended))
		}
		time.Sleep(time.Millisecond)
	}
}
