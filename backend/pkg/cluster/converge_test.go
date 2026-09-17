package cluster

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// ---- a latency-simulating mesh ----
//
// meshHub delivers a frame the instant it is sent, so frames on different
// links always arrive in send order. Real TCP connections do not: a full view
// sent by node-4 before node-2's score announcement can reach node-5 after it,
// because the 4->5 link is slower than the 2->5 link. simNet reproduces that. A
// frame is due at send time plus its link's latency, and flush delivers frames
// in due order, not send order. Everything is driven by the test and a seeded
// PRNG, so a run is exactly repeatable.

// simStep is the clock granularity: the failure-detector tick. Every other
// timer the node runs is a multiple of it.
const simStep = DefaultHeartbeatInterval

type simFrame struct {
	due      time.Duration
	seq      uint64
	from, to protocol.NodeID
	env      *protocol.Envelope
}

type simNet struct {
	mu    sync.Mutex
	now   time.Duration
	seq   uint64
	rng   *rand.Rand
	delay func(from, to protocol.NodeID) time.Duration
	queue []simFrame
	// last is the latest due time per directed link. TCP never reorders
	// within a connection, so a frame is never due before the previous frame
	// on its own link; reordering only happens across links.
	last  map[[2]protocol.NodeID]time.Duration
	nodes map[protocol.NodeID]*Node
	down  map[protocol.NodeID]bool
	// sent counts frames by type, for thrash accounting.
	sent map[protocol.MessageType]int
	// drop, if set, loses matching frames at delivery time.
	drop func(simFrame) bool
	// trace, if non-nil, records every enqueued frame, for pinpointing where
	// two runs of one seed part ways.
	trace *[]string
}

func (s *simNet) setDrop(fn func(simFrame) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = fn
}

type simEndpoint struct {
	self   protocol.NodeID
	net    *simNet
	events chan network.PeerEvent
}

func (e *simEndpoint) Self() protocol.NodeID { return e.self }

func (e *simEndpoint) Send(_ context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	s := e.net
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down[e.self] {
		return nil // a crashed process sends nothing
	}
	if _, ok := s.nodes[to]; !ok || s.down[to] {
		return network.ErrUnknownPeer
	}
	s.enqueueLocked(e.self, to, env)
	return nil
}

func (e *simEndpoint) Broadcast(_ context.Context, env *protocol.Envelope) int {
	s := e.net
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down[e.self] {
		return 0
	}
	n := 0
	for _, to := range s.idsLocked() {
		if to != e.self && !s.down[to] {
			s.enqueueLocked(e.self, to, env)
			n++
		}
	}
	return n
}

func (e *simEndpoint) Peers() []network.PeerInfo        { return nil }
func (e *simEndpoint) Events() <-chan network.PeerEvent { return e.events }

func (s *simNet) idsLocked() []protocol.NodeID {
	out := make([]protocol.NodeID, 0, len(s.nodes))
	for id := range s.nodes {
		out = append(out, id)
	}
	sortIDs(out)
	return out
}

func (s *simNet) enqueueLocked(from, to protocol.NodeID, env *protocol.Envelope) {
	s.seq++
	s.sent[env.Type]++
	due := s.now + s.delay(from, to)
	link := [2]protocol.NodeID{from, to}
	if prev := s.last[link]; due < prev {
		due = prev
	}
	s.last[link] = due
	if s.trace != nil {
		*s.trace = append(*s.trace, fmt.Sprintf("#%d t=%v %s->%s %s due=%v %s", s.seq, s.now, from, to, env.Type, due, env.Payload))
	}
	s.queue = append(s.queue, simFrame{due: due, seq: s.seq, from: from, to: to, env: env})
}

// pop removes and returns the earliest frame, by due time then send order.
func (s *simNet) pop() (simFrame, *Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return simFrame{}, nil, false
	}
	sort.Slice(s.queue, func(i, j int) bool {
		if s.queue[i].due != s.queue[j].due {
			return s.queue[i].due < s.queue[j].due
		}
		return s.queue[i].seq < s.queue[j].seq
	})
	f := s.queue[0]
	s.queue = s.queue[1:]
	if f.due > s.now {
		s.now = f.due
	}
	if s.down[f.to] || (s.drop != nil && s.drop(f)) {
		return f, nil, true
	}
	return f, s.nodes[f.to], true
}

type simNode struct {
	id    protocol.NodeID
	node  *Node
	ep    *simEndpoint
	hs    *fakeHealth
	clock *FakeClock
	stop  context.CancelFunc
	done  chan struct{}
}

type sim struct {
	t     *testing.T
	ctx   context.Context
	net   *simNet
	nodes []*simNode
	rng   *rand.Rand
	// base is the symmetric per-link RTT in ms. Probe scores and frame delays
	// are both derived from it.
	base map[protocol.NodeID]map[protocol.NodeID]float64
	// jitter is the +/- amplitude, in ms, applied to every probe sample.
	jitter  float64
	elapsed time.Duration
	// strays are the (leader, worker) pairs the previous check found listed
	// by a leader the worker is not attached to.
	strays map[[2]protocol.NodeID]bool
	// pointed are the (leader, worker) pairs where the worker named that
	// leader as its own at some point since the previous check, sampled after
	// every tick and every delivered frame. See check.
	pointed map[[2]protocol.NodeID]bool
}

// observe records whom id currently names as its leader. Called only when
// id's loop has just settled, so the published status is current.
func (s *sim) observe(id protocol.NodeID, n *Node) {
	n.statusMu.RLock()
	l := n.status.Leader
	n.statusMu.RUnlock()
	if l == "" || l == id {
		return
	}
	if s.pointed == nil {
		s.pointed = make(map[[2]protocol.NodeID]bool)
	}
	s.pointed[[2]protocol.NodeID{l, id}] = true
}

// newSim starts one Node per id over a simNet with seeded per-link latencies
// drawn from [lo, hi] ms. Nodes use the production defaults except where
// mutate says otherwise; Election is exactly what cmd/swarm-node passes.
func newSim(t *testing.T, seed uint64, idList []protocol.NodeID, lo, hi float64, mutate func(*NodeConfig)) *sim {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	s := &sim{t: t, ctx: ctx, rng: rng, base: make(map[protocol.NodeID]map[protocol.NodeID]float64)}
	for _, a := range idList {
		s.base[a] = make(map[protocol.NodeID]float64)
	}
	for i, a := range idList {
		for _, b := range idList[i+1:] {
			rtt := lo + rng.Float64()*(hi-lo)
			s.base[a][b], s.base[b][a] = rtt, rtt
		}
	}
	s.net = &simNet{
		rng:   rng,
		last:  make(map[[2]protocol.NodeID]time.Duration),
		nodes: make(map[protocol.NodeID]*Node),
		down:  make(map[protocol.NodeID]bool),
		sent:  make(map[protocol.MessageType]int),
	}
	// Frame delay: the link's RTT, plus up to 1ms of queueing noise. Drawn
	// under the net lock, in deterministic enqueue order.
	s.net.delay = func(from, to protocol.NodeID) time.Duration {
		ms := s.base[from][to] + s.net.rng.Float64()
		return time.Duration(ms * float64(time.Millisecond))
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	for _, id := range idList {
		ep := &simEndpoint{self: id, net: s.net, events: make(chan network.PeerEvent, 256)}
		hs := newFakeHealth()
		clock := NewFakeClock(epoch)
		cfg := NodeConfig{
			Self:        id,
			Advertise:   addrOf(id),
			Incarnation: 1,
			Transport:   ep,
			Health:      hs,
			Clock:       clock,
			// What cmd/swarm-node passes today: a threshold and nothing else.
			Election:     Config{Threshold: DefaultThreshold},
			ProbeTimeout: probeEvery / 2,
			Logger:       quietLogger(),
			Shuffle: func(ids []protocol.NodeID) {
				// Called on a loop goroutine at a deterministic point; the lock
				// serialises it with delay draws.
				s.net.mu.Lock()
				s.net.rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
				s.net.mu.Unlock()
			},
		}
		if mutate != nil {
			mutate(&cfg)
		}
		n, err := NewNode(cfg)
		if err != nil {
			t.Fatal(err)
		}
		nctx, nstop := context.WithCancel(ctx)
		sn := &simNode{id: id, node: n, ep: ep, hs: hs, clock: clock, stop: nstop, done: make(chan struct{})}
		s.nodes = append(s.nodes, sn)
		wg.Add(1)
		go func() { defer wg.Done(); defer close(sn.done); _ = n.Run(nctx) }()
		if err := n.settle(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.net.mu.Lock()
	for _, sn := range s.nodes {
		s.net.nodes[sn.id] = sn.node
	}
	s.net.mu.Unlock()
	s.resample()
	// Full mesh. One node at a time, settled before the next is told
	// anything: pushing every node's events first and settling afterwards
	// would let six loops react concurrently, and their sends would race for
	// the shared PRNG and the frame order.
	for _, a := range s.nodes {
		for _, b := range s.nodes {
			if a != b {
				a.ep.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: b.id, Advertise: addrOf(b.id), Incarnation: 1}}
			}
		}
		if err := a.node.settle(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.flush()
	return s
}

// resample draws a fresh probe result for every live link.
func (s *sim) resample() {
	for _, a := range s.nodes {
		for _, b := range s.nodes {
			if a == b {
				continue
			}
			if s.net.isDown(b.id) {
				continue
			}
			v := s.base[a.id][b.id] + (s.rng.Float64()*2-1)*s.jitter
			if v < 0.01 {
				v = 0.01
			}
			a.hs.set(addrOf(b.id), v)
		}
	}
}

func (s *simNet) isDown(id protocol.NodeID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down[id]
}

func (s *sim) live() []*simNode {
	var out []*simNode
	for _, sn := range s.nodes {
		if !s.net.isDown(sn.id) {
			out = append(out, sn)
		}
	}
	return out
}

func (s *sim) settleAll() {
	s.t.Helper()
	for _, sn := range s.live() {
		if err := sn.node.settle(s.ctx, nil); err != nil {
			s.t.Fatalf("settle %s: %v", sn.id, err)
		}
	}
}

// flush delivers every queued frame in due order, settling the receiver after
// each one so processing order is the delivery order.
func (s *sim) flush() {
	s.t.Helper()
	for i := 0; ; i++ {
		if i > 200000 {
			s.net.mu.Lock()
			sent := fmt.Sprint(s.net.sent)
			s.net.mu.Unlock()
			s.t.Fatalf("network never went quiet at t=%v; frames sent so far by type: %s", s.elapsed, sent)
		}
		f, target, ok := s.net.pop()
		if !ok {
			// A node may still be finishing a frame it dequeued; settle and
			// look again before calling the network quiet.
			s.settleAll()
			if s.net.queued() == 0 {
				return
			}
			continue
		}
		if target == nil {
			continue
		}
		target.Handler()(f.from, f.env)
		if err := target.settle(s.ctx, nil); err != nil {
			s.t.Fatalf("settle %s: %v", f.to, err)
		}
		s.observe(f.to, target)
	}
}

func (s *simNet) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// run advances every live clock by d in simStep increments. Each step fires
// the ticks due on every node, then lets the network carry what they sent.
// Probe results are re-drawn once per probe period, just before it fires.
func (s *sim) run(d time.Duration, each func()) {
	s.t.Helper()
	for step := time.Duration(0); step < d; step += simStep {
		s.elapsed += simStep
		if s.elapsed%probeEvery == 0 {
			s.resample()
		}
		s.net.mu.Lock()
		s.net.now += simStep
		s.net.mu.Unlock()
		for _, sn := range s.live() {
			// Tick by tick, settling after each one, not one Advance and one
			// settle: several of the node's tickers share this deadline, and
			// with a single Advance the loop would choose at random between the
			// next tick and the result of the probe round the previous tick
			// started. That choice decides whether this instant's gossip and
			// beats carry the new self-score, and in what order the shared
			// PRNG is drawn, so the run would depend on the scheduler rather
			// than the seed. See FakeClock.advanceStepwise.
			// Advancing reaches the loop only through ticks, each settled
			// here, so nothing is left to settle once it returns.
			sn.clock.advanceStepwise(simStep, func() {
				if err := sn.node.settle(s.ctx, nil); err != nil {
					s.t.Fatalf("settle %s: %v", sn.id, err)
				}
				s.observe(sn.id, sn.node)
			})
		}
		s.flush()
		if each != nil {
			each()
		}
	}
}

// kill crashes a node: everything it had queued or was being sent is lost, it
// sends nothing more, every peer sees its socket reset (PeerDown PeerDied), and
// probes of it fail.
func (s *sim) kill(id protocol.NodeID) {
	s.t.Helper()
	s.net.mu.Lock()
	s.net.down[id] = true
	kept := s.net.queue[:0]
	for _, f := range s.net.queue {
		if f.from != id && f.to != id {
			kept = append(kept, f)
		}
	}
	s.net.queue = kept
	s.net.mu.Unlock()
	for _, sn := range s.nodes {
		if sn.id == id {
			sn.stop()
			<-sn.done
			continue
		}
		if s.net.isDown(sn.id) {
			continue
		}
		sn.hs.fail(addrOf(id), fmt.Errorf("sim: %s is down", id))
		sn.ep.events <- network.PeerEvent{Kind: network.PeerDown, Peer: network.PeerInfo{ID: id, Advertise: addrOf(id)}, Disposition: network.DispositionPeerDied}
		// Settled before the next survivor hears of the crash, for the same
		// reason newSim introduces the mesh one node at a time.
		if err := sn.node.settle(s.ctx, nil); err != nil {
			s.t.Fatalf("settle %s: %v", sn.id, err)
		}
		s.observe(sn.id, sn.node)
	}
	s.flush()
}

func (s *sim) node(id protocol.NodeID) *simNode {
	for _, sn := range s.nodes {
		if sn.id == id {
			return sn
		}
	}
	s.t.Fatalf("no node %s", id)
	return nil
}

// agreement describes the swarm at a quiet moment.
type agreement struct {
	leaders []protocol.NodeID
	// problems lists every way the swarm disagrees or is not fully attached.
	problems []string
}

// check reports whether every live node holds the same leader set and the same
// reported scores for every live member, whether every node's own role matches
// that set, and whether every worker is attached to an elected leader that
// lists it.
func (s *sim) check() agreement {
	live := s.live()
	var a agreement
	sts := make(map[protocol.NodeID]Status, len(live))
	for _, sn := range live {
		sts[sn.id] = sn.node.Status()
	}
	first := sts[live[0].id]
	a.leaders = first.Leaders
	seen := make(map[[2]protocol.NodeID]bool)
	defer func() { s.strays, s.pointed = seen, nil }()
	for _, sn := range live {
		st := sts[sn.id]
		if !equalIDs(st.Leaders, first.Leaders) {
			a.problems = append(a.problems, fmt.Sprintf("%s leaders %v != %s leaders %v", sn.id, st.Leaders, live[0].id, first.Leaders))
		}
		for _, other := range live {
			if got, want := st.Reported[other.id], sts[other.id].Reported[other.id]; got != want {
				a.problems = append(a.problems, fmt.Sprintf("%s holds %s's score %v, %s reports %v", sn.id, other.id, got, other.id, want))
			}
		}
	}
	if len(a.problems) > 0 {
		return a
	}
	for _, sn := range live {
		st := sts[sn.id]
		if contains(first.Leaders, sn.id) {
			if st.Role != RoleLeader || st.Leader != sn.id {
				a.problems = append(a.problems, fmt.Sprintf("%s is elected but reports %s", sn.id, st))
			}
			// A leader may still list a worker that re-homed since its last
			// beat; the next beat's ack drops it. It must not still list it
			// at the next check, one beat later -- unless the worker named
			// this leader again in between (a fresh JOIN, abandoned within
			// the same instant). Then the listing was true when that beat
			// was acked, this is a new stale episode, and it gets its own
			// beat. Without that distinction a worker bouncing A -> B -> A
			// -> C across two instants reads as one listing never dropped.
			for _, w := range st.Attached {
				key := [2]protocol.NodeID{sn.id, w}
				if sts[w].Leader == sn.id {
					continue
				}
				seen[key] = true
				if s.strays[key] && !s.pointed[key] {
					a.problems = append(a.problems, fmt.Sprintf("leader %s still lists %s, which is attached to %q", sn.id, w, sts[w].Leader))
				}
			}
			continue
		}
		if st.Role != RoleWorker || st.Leader == sn.id {
			a.problems = append(a.problems, fmt.Sprintf("%s is not elected but reports %s", sn.id, st))
		}
		if st.Leader == "" || !contains(first.Leaders, st.Leader) {
			a.problems = append(a.problems, fmt.Sprintf("worker %s attached to %q", sn.id, st.Leader))
			continue
		}
		if !contains(sts[st.Leader].Attached, sn.id) {
			a.problems = append(a.problems, fmt.Sprintf("worker %s thinks it is attached to %s, which lists %v", sn.id, st.Leader, sts[st.Leader].Attached))
		}
	}
	return a
}

// dump renders every node's view of the swarm, one block per node, for failure
// messages. A flake in a seeded simulator is only actionable if the report says
// which node believed what: its own scores, the Seq it holds for every member,
// its leader set and who it thinks is attached to whom.
func (s *sim) dump() string {
	var b strings.Builder
	s.net.mu.Lock()
	fmt.Fprintf(&b, "sim t=%v net.now=%v queued=%d sent=%v\n", s.elapsed, s.net.now, len(s.net.queue), s.net.sent)
	s.net.mu.Unlock()
	for _, sn := range s.nodes {
		if s.net.isDown(sn.id) {
			fmt.Fprintf(&b, "  %s: down\n", sn.id)
			continue
		}
		st := sn.node.Status()
		fmt.Fprintf(&b, "  %s: role=%s term=%d leader=%q leaders=%v attached=%v view=v%d\n",
			sn.id, st.Role, st.Term, st.Leader, st.Leaders, st.Attached, st.View.Version)
		for _, m := range st.View.Members {
			fmt.Fprintf(&b, "    %s role=%s state=%s inc=%d seq=%d score=%.4f local=%.4f claim=%+v\n",
				m.ID, m.Role, m.State, m.Incarnation, m.Seq, m.Score, st.Scores[m.ID], st.Claims[m.ID])
		}
	}
	return b.String()
}

func sixIDs() []protocol.NodeID {
	return ids("node-1", "node-2", "node-3", "node-4", "node-5", "node-6")
}

// fingerprint is everything a run's outcome depends on: the final per-node
// state, the frame counts, the simulated time and how many frames were sent.
func (s *sim) fingerprint() string {
	s.net.mu.Lock()
	seq := s.net.seq
	s.net.mu.Unlock()
	return fmt.Sprintf("frames=%d\n%s", seq, s.dump())
}

// The simulator is only useful if a seed pins the run down. Two runs of one
// seed must send the same frames, with the same delays, and end in the same
// state, whatever the Go scheduler does.
//
// Two things used to break that, and under CPU load they made
// TestSwarmAgreesAtEveryQuietMomentUnderReordering fail on seeds that passed
// on an idle machine:
//   - Several tickers firing at one instant let the loop choose at random
//     between the next tick and a finished probe round (run now settles
//     after every tick; see FakeClock.advanceStepwise).
//   - newSim and kill queued PeerUp/PeerDown events on every node before
//     settling any, so six loops reacted concurrently and raced for the
//     shared PRNG (they are now introduced one node at a time).
//
// Run with -race and -cpu 1,2,4 to give the scheduler room to differ.
func TestSimIsDeterministicPerSeed(t *testing.T) {
	scenarios := []struct {
		name string
		play func(t *testing.T, seed uint64, trace *[]string) *sim
	}{
		{"reordering", func(t *testing.T, seed uint64, trace *[]string) *sim {
			s := newSim(t, seed, sixIDs(), 0.2, 3, nil)
			s.traceInto(trace)
			s.jitter = 1
			s.run(20*time.Second, nil)
			return s
		}},
		{"leader crash with tasks", func(t *testing.T, seed uint64, trace *[]string) *sim {
			var r results
			s := newSim(t, seed, fiveIDs(), 0.2, 3, func(c *NodeConfig) { c.OnTaskResult = r.record })
			s.traceInto(trace)
			s.jitter = DefaultHysteresis / 5
			s.run(15*time.Second, nil)
			victim := s.check().leaders[0]
			for i := 0; i < 6; i++ {
				if err := s.node(victim).node.SubmitTask(s.ctx, echo(fmt.Sprintf("task-%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			s.settleAll()
			s.flush()
			s.kill(victim)
			s.run(failoverBound+5*time.Second, nil)
			return s
		}},
	}
	const runs = 3
	for _, sc := range scenarios {
		for _, seed := range []uint64{8, 9} {
			t.Run(fmt.Sprintf("%s/seed=%d", sc.name, seed), func(t *testing.T) {
				var (
					first      string
					firstTrace []string
				)
				for i := 0; i < runs; i++ {
					var trace []string
					got := sc.play(t, seed, &trace).fingerprint()
					if i == 0 {
						first, firstTrace = got, trace
						continue
					}
					if got == first && slices.Equal(trace, firstTrace) {
						continue
					}
					at := 0
					for at < len(trace) && at < len(firstTrace) && trace[at] == firstTrace[at] {
						at++
					}
					window := func(tr []string) string {
						return strings.Join(tr[min(len(tr), max(0, at-3)):min(len(tr), at+3)], "\n")
					}
					t.Fatalf("seed %d: run %d diverged from run 0 at frame %d\n--- run 0 frames:\n%s\n--- run %d frames:\n%s\n--- run 0:\n%s\n--- run %d:\n%s",
						seed, i, at, window(firstTrace), i, window(trace), first, i, got)
				}
			})
		}
	}
}

// traceInto records every frame sent from now on into trace.
func (s *sim) traceInto(trace *[]string) {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	s.net.trace = trace
}

// ---- the regression ----

// The Docker divergence, reproduced. Six nodes, per-link RTTs between 0.2ms
// and 3ms, probe samples jittering by +/-1ms, so self-reports move and are
// re-announced every few seconds. Frames arrive in latency order, so a
// full view carrying an old score regularly lands after the announcement that
// replaced it.
//
// Whatever the scores do, at every quiet moment every node must agree on every
// member's reported score and therefore on the leader set, and each node's own
// role must match it.
//
// Before the fix this failed on every seed: relayed full views overwrote newer
// scores (no Seq), and disagreeing nodes bounced a worker's JOIN between them
// ~100k times in one instant.
func TestSwarmAgreesAtEveryQuietMomentUnderReordering(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 31, 32, 33, 34, 35, 36} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			s := newSim(t, seed, sixIDs(), 0.2, 3, nil)
			s.jitter = 1
			s.run(10*time.Second, nil) // warm-up: every node measured and announced
			var bad []string
			var first string
			s.run(60*time.Second, func() {
				if a := s.check(); len(a.problems) > 0 && len(bad) < 5 {
					bad = append(bad, fmt.Sprintf("t=%v: %v", s.elapsed, a.problems))
					if first == "" {
						first = s.dump()
					}
				}
			})
			if len(bad) > 0 {
				t.Fatalf("seed %d: swarm disagreed at quiet moments:\n%v\nstate at the first one:\n%s", seed, bad, first)
			}
		})
	}
}

// Jitter smaller than the hysteresis margin must not move leadership at all:
// once converged, the leader set is fixed and no node re-elects. The nodes are
// configured exactly as cmd/swarm-node configures them (threshold only), which
// before the fix meant no hysteresis at all and ~35 re-elections per node per
// minute.
func TestSwarmDoesNotThrashOnSubMarginJitter(t *testing.T) {
	for _, seed := range []uint64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			s := newSim(t, seed, sixIDs(), 0.2, 3, nil)
			s.jitter = DefaultHysteresis / 5
			s.run(15*time.Second, nil)
			a := s.check()
			if len(a.problems) > 0 {
				t.Fatalf("seed %d: not converged after 15s: %v\n%s", seed, a.problems, s.dump())
			}
			if want := LeaderCount(6, DefaultThreshold); len(a.leaders) != want {
				t.Fatalf("leaders = %v, want %d", a.leaders, want)
			}
			// Every leader-set change is broadcast, so the ELECTION_RESULT count
			// is the re-election count. (Terms are not: they also move when a
			// node adopts a higher term over a heartbeat.)
			results := s.net.sentCount(protocol.TypeElectionResult)
			s.run(60*time.Second, func() {
				if b := s.check(); len(b.problems) > 0 || !equalIDs(b.leaders, a.leaders) {
					t.Fatalf("seed %d: t=%v: leaders moved %v -> %v (%v)\n%s", seed, s.elapsed, a.leaders, b.leaders, b.problems, s.dump())
				}
			})
			if got := s.net.sentCount(protocol.TypeElectionResult); got != results {
				t.Errorf("%d ELECTION_RESULT broadcasts under sub-margin jitter", got-results)
			}
		})
	}
}

func (s *simNet) sentCount(t protocol.MessageType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[t]
}

// The mechanism, isolated. node-4's full view is captured while it still
// holds node-2's old score, node-2 then announces a new one to everybody, and
// the stale view reaches node-5 last. node-5 must keep the newer score.
func TestStaleRelayedScoreDoesNotOverwriteFresherOne(t *testing.T) {
	ctx, hub, nodes := newMesh(t, ids("n1", "n2", "n3"), map[protocol.NodeID]map[protocol.NodeID]float64{
		"n1": {"n2": 1, "n3": 1},
		"n2": {"n1": 1, "n3": 1},
		"n3": {"n1": 1, "n2": 1},
	}, nil)
	_ = hub
	n1, n2, n3 := nodes[0], nodes[1], nodes[2]
	fullMesh(t, ctx, nodes)
	advanceMesh(t, ctx, nodes, probeEvery)
	if got := memberOf(t, n3, "n2").Score; got != 1 {
		t.Fatalf("setup: n3 holds n2 = %v, want 1", got)
	}

	// n1's full view, as it stands now: n2 reports 1.
	stale := captureView(t, n1)

	// n2's links get slower; it re-measures and announces 4.
	n2.hs.set(addrOf("n1"), 4)
	n2.hs.set(addrOf("n3"), 4)
	n2.clock.Advance(probeEvery)
	quiesce(t, ctx, nodes)
	if got := memberOf(t, n3, "n2").Score; got != 4 {
		t.Fatalf("n3 did not hear n2's announcement: %v", got)
	}

	// The old view arrives late.
	n3.node.Handler()("n1", stale)
	quiesce(t, ctx, nodes)
	if got := memberOf(t, n3, "n2").Score; got != 4 {
		t.Fatalf("a stale relayed score overwrote a fresher one: n3 holds n2 = %v, want 4", got)
	}
}

// captureView builds the MEMBERSHIP_DELTA m would send as anti-entropy right
// now, without sending it.
func captureView(t *testing.T, m *meshNode) *protocol.Envelope {
	t.Helper()
	var env *protocol.Envelope
	if err := m.node.barrier(context.Background(), func() {
		view := m.node.table.Snapshot()
		recs := make([]protocol.MemberRecord, 0, len(view.Members))
		for _, mem := range view.Members {
			recs = append(recs, memberRecord(mem))
		}
		var err error
		env, err = protocol.NewEnvelope(protocol.TypeMembershipDelta, m.id, "", protocol.MembershipDeltaPayload{Members: recs, ViewVersion: view.Version})
		if err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return env
}

// ---- the individual fixes ----

// deltaAbout delivers from's MEMBERSHIP_DELTA carrying one Seq-ordered record.
func (h *harness) deltaAbout(from, id protocol.NodeID, role Role, score float64, seq uint64) {
	h.t.Helper()
	h.frame(from, protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: id, Advertise: addrOf(id), Incarnation: 1, Role: role.String(), State: "alive", Score: score, Seq: seq,
	}}})
}

// With Seq, a relayed claim is as good as a first-hand one: the newer one wins,
// whoever carries it, and an older one loses, whoever carries it.
func TestRelayedClaimsAreOrderedBySeq(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.deltaAbout("node-c", "node-c", RoleLeader, 0.5, 3)
	h.deltaAbout("node-b", "node-c", RoleWorker, 0.7, 4) // newer, relayed
	if m := h.member("node-c"); m.Role != RoleWorker || m.Score != 0.7 || m.Seq != 4 {
		t.Fatalf("newer relayed claim not applied: %+v", m)
	}
	h.deltaAbout("node-c", "node-c", RoleLeader, 0.5, 3) // older, first-hand
	if m := h.member("node-c"); m.Role != RoleWorker || m.Score != 0.7 {
		t.Fatalf("older first-hand claim applied: %+v", m)
	}
	// A new member introduced by a relay keeps the claim the relay carried.
	h.deltaAbout("node-b", "node-d", RoleLeader, 0.1, 2)
	if m := h.member("node-d"); m.Role != RoleLeader {
		t.Fatalf("relayed ordered claim ignored for a new member: %+v", m)
	}
}

// An acceptance that no longer matches what we are doing is ignored.
func TestStaleJoinAckIsIgnored(t *testing.T) {
	t.Run("already promoted", func(t *testing.T) {
		h := newHarness(t, "node-a", nil)
		h.peerUp("node-b", 1)
		h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
		if st := h.status(); st.Leader != "node-a" || st.Role != RoleLeader {
			t.Fatalf("a leader was re-pointed by a late ack: %s", st)
		}
	})
	t.Run("moved on", func(t *testing.T) {
		h := threeNodes(t, "node-z") // provisionally joining node-b
		h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-c"})
		if st := h.status(); st.Leader != "node-b" {
			t.Fatalf("Leader = %q, want node-b kept", st.Leader)
		}
	})
}

// Two nodes that each believe the other leads bounce a worker between them.
// The worker answers at most maxRejectionsPerRound rejections with a JOIN,
// then waits for the next probe round.
func TestRejectionLoopIsBounded(t *testing.T) {
	h := newHarness(t, "node-z", func(c *NodeConfig) { c.Election.Threshold = 0.5 })
	for _, id := range ids("node-b", "node-c", "node-d") {
		h.hs.set(addrOf(id), 1.0)
		h.peerUp(id, 1)
	}
	h.report("node-b", 1, 1.0)
	h.report("node-c", 1, 1.0)
	h.report("node-d", 1, 10.0)
	h.probeRound()
	requireIDs(t, "Leaders", h.status().Leaders, ids("node-b", "node-c"))
	base := len(h.tr.sentOf(protocol.TypeJoinCluster))

	for i := 0; i < 20; i++ {
		from, hint := protocol.NodeID("node-b"), protocol.NodeID("node-c")
		if i%2 == 1 {
			from, hint = hint, from
		}
		h.frame(from, protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: false, Reason: "not a leader", Leader: hint})
	}
	if got := len(h.tr.sentOf(protocol.TypeJoinCluster)) - base; got != maxRejectionsPerRound {
		t.Fatalf("JOINs answered to 20 rejections = %d, want %d", got, maxRejectionsPerRound)
	}
	if st := h.status(); st.Leader != "" {
		t.Fatalf("Leader = %q after giving up, want detached", st.Leader)
	}
	h.probeRound()
	if got := len(h.tr.sentOf(protocol.TypeJoinCluster)) - base; got != maxRejectionsPerRound+1 {
		t.Fatalf("no fresh JOIN after the next round: %d", got)
	}
}

// A leader that steps down forgets its workers. A worker whose own election
// never dropped that leader must notice the step-down claim and re-JOIN, or it
// stays "attached" to a leader that does not list it.
func TestWorkerRejoinsLeaderThatSteppedDown(t *testing.T) {
	h := threeNodes(t, "node-z")
	h.deltaAbout("node-b", "node-b", RoleLeader, 1.0, 10)
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	base := len(h.tr.sentOf(protocol.TypeJoinCluster))

	h.deltaAbout("node-b", "node-b", RoleWorker, 1.0, 11) // steps down; still best
	requireIDs(t, "Leaders", h.status().Leaders, ids("node-b"))
	joins := h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != base+1 || joins[base].to != "node-b" {
		t.Fatalf("no re-JOIN after the step-down: %+v", joins[base:])
	}

	// A step-down claim older than what we hold does nothing.
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	h.deltaAbout("node-b", "node-b", RoleLeader, 1.0, 12)
	h.deltaAbout("node-c", "node-b", RoleWorker, 1.0, 11)
	if got := len(h.tr.sentOf(protocol.TypeJoinCluster)); got != base+1 {
		t.Fatalf("a stale step-down triggered %d JOINs", got-base-1)
	}
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("Leader = %q, want node-b", st.Leader)
	}
}

// ackJoin delivers from's answer to one specific JOIN, the way a real leader
// does: echoing that JOIN's envelope ID.
func (h *harness) ackJoin(join *protocol.Envelope, p protocol.JoinAckPayload) {
	h.t.Helper()
	env, err := protocol.NewReply(join, protocol.TypeJoinAck, join.To, p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.node.Handler()(join.To, env)
	h.settle()
}

// Seed 501 of the reordering sweep, reduced. A worker JOINs node-b while
// node-b is briefly not leading, then JOINs it again once it is. The answers
// arrive in order: "rejected, not a leader" for the first JOIN, then
// "accepted" for the second. The rejection answers a JOIN we are no longer
// waiting on, so it must neither detach us nor spend the rejection budget, and
// the acceptance must attach us. Before the fix the rejection, matched by
// sender alone, detached the worker; with the budget spent it did not re-JOIN,
// and the acceptance was then dropped as stale. node-b listed a worker that
// believed it was detached until the next probe round.
func TestRejectionOfAnEarlierJoinDoesNotVoidTheCurrentOne(t *testing.T) {
	h := threeNodes(t, "node-z") // JOIN #1 to node-b outstanding
	joins := h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 1 || joins[0].to != "node-b" {
		t.Fatalf("setup: JOINs = %+v, want one to node-b", joins)
	}
	first := joins[0].env

	// node-b claims the seat and then (by Seq) gives it up: the attachment is
	// void and we JOIN it again.
	h.deltaAbout("node-b", "node-b", RoleLeader, 1.0, 10)
	h.deltaAbout("node-b", "node-b", RoleWorker, 1.0, 11)
	joins = h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 2 || joins[1].to != "node-b" {
		t.Fatalf("setup: JOINs = %+v, want a second one to node-b", joins)
	}
	second := joins[1].env
	// This round's rejections are already spent, as they were in the sim: a
	// detach now is not repaired until the next probe round.
	if err := h.node.barrier(h.ctx, func() { h.node.rejections = maxRejectionsPerRound }); err != nil {
		t.Fatal(err)
	}

	h.ackJoin(first, protocol.JoinAckPayload{Accepted: false, Reason: "not a leader", Leader: "node-c"})
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("a rejection of the earlier JOIN detached us: Leader = %q", st.Leader)
	}
	h.ackJoin(second, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("the acceptance of the current JOIN was dropped: Leader = %q", st.Leader)
	}
	if got := len(h.tr.sentOf(protocol.TypeJoinCluster)); got != 2 {
		t.Fatalf("%d JOINs, want 2: the stale answer caused a re-JOIN", got)
	}

	// Answers to a JOIN already answered change nothing either way.
	h.ackJoin(second, protocol.JoinAckPayload{Accepted: false, Reason: "not a leader", Leader: "node-c"})
	h.ackJoin(first, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("a repeated answer moved us: Leader = %q", st.Leader)
	}
}

// A leader drops a worker for promotion only when the worker claims it, not
// because the leader's own election momentarily includes the worker.
func TestLeaderKeepsWorkerUntilItClaimsLeadership(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) { c.Election.Threshold = 0.5 })
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 1.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.deltaAbout("node-b", "node-b", RoleWorker, 9, 2)
	h.deltaAbout("node-c", "node-c", RoleWorker, 9, 2)
	h.probeRound()
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	// node-b's report improves: our election now seats it, but it has not
	// claimed the seat yet.
	h.deltaAbout("node-b", "node-b", RoleWorker, 0.1, 3)
	st := h.status()
	if !contains(st.Leaders, "node-b") || !contains(st.Leaders, "node-a") {
		t.Fatalf("setup: Leaders = %v, want node-a and node-b", st.Leaders)
	}
	requireIDs(t, "Attached before the claim", st.Attached, ids("node-b"))

	h.deltaAbout("node-b", "node-b", RoleLeader, 0.1, 4)
	requireIDs(t, "Attached after the claim", h.status().Attached, nil)
}

// cmd/swarm-node sets only the threshold. That must not mean "no damping".
func TestZeroElectionHysteresisTakesTheDefault(t *testing.T) {
	c := NodeConfig{Election: Config{Threshold: 0.3}}.withDefaults()
	if c.Election.Hysteresis != DefaultHysteresis {
		t.Fatalf("Hysteresis = %v, want %v", c.Election.Hysteresis, DefaultHysteresis)
	}
	c = NodeConfig{Election: Config{Hysteresis: 0.01}}.withDefaults()
	if c.Election.Hysteresis != 0.01 {
		t.Fatalf("an explicit margin was overridden: %v", c.Election.Hysteresis)
	}
}
