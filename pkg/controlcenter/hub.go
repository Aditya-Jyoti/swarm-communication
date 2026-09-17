package controlcenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
	"swarm-net/pkg/telemetry"
)

// Request errors, mapped to HTTP statuses by the handlers.
var (
	// ErrBadRequest wraps every validation failure (400).
	ErrBadRequest = errors.New("bad request")
	// ErrNoLeader means no connected node currently reports role "leader" (503).
	ErrNoLeader = errors.New("no connected leader")
	// ErrUnknownNode means a chaos target is not connected (404).
	ErrUnknownNode = errors.New("node not connected")
	// ErrUnavailable means the hub is not running (503).
	ErrUnavailable = errors.New("control center not running")
)

// nodeEntry is everything the CC knows about one node. Hub-owned.
type nodeEntry struct {
	id        protocol.NodeID
	link      *nodeLink // nil while disconnected
	tel       protocol.TelemetryPayload
	lastSeen  time.Time
	downSince time.Time
}

// taskEntry is one task. Hub-owned.
type taskEntry struct {
	view TaskView
}

// wsClient is one browser. The hub owns membership of the clients set; out
// is written only by the hub and read only by the client's writer.
type wsClient struct {
	out    chan []byte
	cancel context.CancelFunc
}

// hub is the CC's state. Every field is owned by the hub goroutine; see Server.
type hub struct {
	s        *Server
	nodes    map[protocol.NodeID]*nodeEntry
	tasks    []*taskEntry // newest last
	taskByID map[string]*taskEntry
	nextTask uint64
	rr       uint64
	clients  map[*wsClient]struct{}
	pingSeq  uint64
}

func newHub(s *Server) *hub {
	return &hub{
		s:        s,
		nodes:    make(map[protocol.NodeID]*nodeEntry),
		taskByID: make(map[string]*taskEntry),
		clients:  make(map[*wsClient]struct{}),
	}
}

// run is the hub loop. It returns when ctx is cancelled, after dropping every
// browser and closing every node connection it holds.
func (h *hub) run(ctx context.Context) {
	snap := time.NewTicker(h.s.cfg.SnapshotInterval)
	defer snap.Stop()
	keep := time.NewTicker(h.s.cfg.KeepAlive)
	defer keep.Stop()

	for {
		select {
		case <-ctx.Done():
			// hubDone first: a browser writer woken by the cancel below
			// checks it to choose "going away" over "too slow".
			close(h.s.hubDone)
			for c := range h.clients {
				c.cancel()
			}
			for _, n := range h.nodes {
				if n.link != nil {
					_ = n.link.conn.Close()
				}
			}
			return
		case op := <-h.s.ops:
			op(h)
		case <-snap.C:
			h.expire()
			h.broadcast(h.snapshotBytes())
		case <-keep.C:
			h.keepAlive()
		}
	}
}

func (h *hub) now() time.Time { return h.s.cfg.Now() }

// --- node side -----------------------------------------------------------------

func (h *hub) onConnUp(l *nodeLink) {
	n := h.nodes[l.id]
	if n == nil {
		n = &nodeEntry{id: l.id, tel: defaultTelemetry(l.id)}
		h.nodes[l.id] = n
	}
	old := n.link
	n.link = l
	n.lastSeen = h.now()
	n.downSince = time.Time{}
	if old != nil {
		// Newest wins. The old link's connDown will find it is no longer
		// current and stay silent, so the dashboard sees no flap.
		_ = old.conn.Close()
		h.s.log.Info("node reconnected; replacing old link", "peer", l.id, "remote", l.remote)
		return
	}
	h.s.log.Info("node connected", "peer", l.id, "remote", l.remote)
	h.event(EventNodeUp, l.id, "connected from "+l.remote)
}

func (h *hub) onConnDown(l *nodeLink, d network.Disposition, err error) {
	n := h.nodes[l.id]
	if n == nil || n.link != l {
		return
	}
	n.link = nil
	n.downSince = h.now()
	h.s.log.Info("node disconnected", "peer", l.id, "disposition", d, "err", err)
	h.event(EventNodeDown, l.id, d.String())
}

func (h *hub) onFrame(l *nodeLink, env *protocol.Envelope) {
	n := h.nodes[l.id]
	if n == nil || n.link != l {
		return // a frame from a link that has since been replaced
	}
	n.lastSeen = h.now()
	switch env.Type {
	case protocol.TypeTelemetry:
		p, err := protocol.PayloadOf[protocol.TelemetryPayload](env)
		if err != nil {
			h.s.log.Warn("bad TELEMETRY", "peer", l.id, "err", err)
			return
		}
		h.applyTelemetry(n, p)
	case protocol.TypeTaskResult:
		p, err := protocol.PayloadOf[protocol.TaskResultPayload](env)
		if err != nil {
			h.s.log.Warn("bad TASK_RESULT", "peer", l.id, "err", err)
			return
		}
		h.applyResult(l.id, p)
	}
	// PONG needs nothing beyond the lastSeen update above.
}

func defaultTelemetry(id protocol.NodeID) protocol.TelemetryPayload {
	return protocol.TelemetryPayload{Node: id, Role: "worker", State: "alive"}
}

func (h *hub) applyTelemetry(n *nodeEntry, p protocol.TelemetryPayload) {
	// The link, not the payload, is the authority on who is speaking.
	p.Node = n.id
	if p.Role == "" {
		p.Role = "worker"
	}
	if p.State == "" {
		p.State = "alive"
	}
	prev := n.tel
	n.tel = p
	if prev.Role != p.Role {
		h.event(EventLeaderChange, n.id, prev.Role+" -> "+p.Role)
	}
	if prev.State != p.State {
		h.event(EventStateChange, n.id, prev.State+" -> "+p.State)
	}
}

func (h *hub) applyResult(from protocol.NodeID, r protocol.TaskResultPayload) {
	t := h.taskByID[r.TaskID]
	if t == nil || t.view.State != TaskPending {
		// Unknown (evicted, or from before a CC restart) or already settled:
		// delivery is at-least-once, and the first result wins.
		return
	}
	worker := r.Worker
	if worker == "" {
		worker = from
	}
	t.view.Worker = worker
	t.view.OK = r.OK
	t.view.Output = r.Output
	t.view.DurationMS = r.DurationMS
	detail := r.TaskID + " ok"
	t.view.State = TaskDone
	if !r.OK {
		t.view.State = TaskFailed
		detail = r.TaskID + " failed"
	}
	h.event(EventTaskDone, worker, detail)
}

// expire forgets nodes that have been disconnected for NodeExpiry.
func (h *hub) expire() {
	now := h.now()
	for id, n := range h.nodes {
		if n.link == nil && now.Sub(n.downSince) >= h.s.cfg.NodeExpiry {
			delete(h.nodes, id)
		}
	}
}

func (h *hub) keepAlive() {
	for _, n := range h.nodes {
		if n.link == nil {
			continue
		}
		h.pingSeq++
		env, err := protocol.NewEnvelope(protocol.TypePing, NodeID, n.id, protocol.PingPayload{Nonce: h.pingSeq, Seq: h.pingSeq})
		if err != nil {
			continue
		}
		h.sendAsync(n.link, env)
	}
}

// sendAsync sends a control-plane frame without blocking the hub: such a send
// may wait for queue space (CtrlSendTimeout), and the hub must never wait on
// one slow node.
func (h *hub) sendAsync(l *nodeLink, env *protocol.Envelope) {
	h.s.spawn(func() {
		if err := l.conn.Send(context.Background(), env); err != nil {
			h.s.log.Warn("send failed", "peer", l.id, "type", env.Type, "err", err)
		}
	})
}

// --- requests ------------------------------------------------------------------

// leaders returns connected nodes that report role leader, sorted by ID so the
// round-robin order is stable.
func (h *hub) leaders() []*nodeEntry {
	var out []*nodeEntry
	for _, n := range h.nodes {
		if n.link != nil && n.tel.Role == "leader" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// submitTasks creates count tasks and sends each to a leader, round-robin.
func (h *hub) submitTasks(m ClientMessage) ([]string, error) {
	if m.Kind == "" {
		return nil, fmt.Errorf("%w: task kind is required", ErrBadRequest)
	}
	count := m.Count
	switch {
	case count < 0:
		return nil, fmt.Errorf("%w: count %d must not be negative", ErrBadRequest, count)
	case count == 0:
		count = 1
	case count > MaxTaskCount:
		count = MaxTaskCount
	}
	leaders := h.leaders()
	if len(leaders) == 0 {
		return nil, ErrNoLeader
	}
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		leader := leaders[h.rr%uint64(len(leaders))]
		h.rr++
		h.nextTask++
		id := "t-" + strconv.FormatUint(h.nextTask, 10)
		t := &taskEntry{view: TaskView{
			TaskID:          id,
			Kind:            m.Kind,
			Leader:          leader.id,
			State:           TaskPending,
			SubmittedUnixMS: h.now().UnixMilli(),
		}}
		h.addTask(t)
		ids = append(ids, id)

		env, err := protocol.NewEnvelope(protocol.TypeTask, NodeID, leader.id, protocol.TaskPayload{TaskID: id, Kind: m.Kind, Body: m.Body})
		if err == nil {
			// TASK is data plane: Send never blocks, it sheds when the
			// queue is full. A shed task is failed now rather than left
			// pending for ever.
			err = leader.link.conn.Send(context.Background(), env)
		}
		if err != nil {
			t.view.State = TaskFailed
			t.view.Output = "send to leader failed: " + err.Error()
			h.event(EventTaskDone, leader.id, id+" failed")
		}
	}
	return ids, nil
}

func (h *hub) addTask(t *taskEntry) {
	h.tasks = append(h.tasks, t)
	h.taskByID[t.view.TaskID] = t
	if over := len(h.tasks) - MaxTasks; over > 0 {
		for _, old := range h.tasks[:over] {
			delete(h.taskByID, old.view.TaskID)
		}
		// Copy down rather than reslice, so the backing array does not grow
		// without bound behind a moving window.
		h.tasks = append(h.tasks[:0], h.tasks[over:]...)
	}
}

// chaos validates and forwards one chaos instruction.
func (h *hub) chaos(m ClientMessage) error {
	c, err := telemetry.ParseChaos(protocol.ChaosPayload{Action: m.Action, DelayMS: m.DelayMS})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if m.Node == "" {
		return fmt.Errorf("%w: chaos node is required", ErrBadRequest)
	}
	n := h.nodes[m.Node]
	if n == nil || n.link == nil {
		return fmt.Errorf("%w: %q", ErrUnknownNode, m.Node)
	}
	env, err := protocol.NewEnvelope(protocol.TypeChaos, NodeID, n.id, protocol.ChaosPayload{Action: string(c.Action), DelayMS: int(c.Delay.Milliseconds())})
	if err != nil {
		return err
	}
	h.sendAsync(n.link, env)
	detail := string(c.Action)
	if c.Action == telemetry.ChaosDelay {
		detail += " " + c.Delay.String()
	}
	h.event(EventChaos, n.id, detail)
	return nil
}

// --- browsers ------------------------------------------------------------------

func (h *hub) addClient(c *wsClient) {
	h.clients[c] = struct{}{}
	if b := h.snapshotBytes(); b != nil {
		h.sendTo(c, b)
	}
}

func (h *hub) removeClient(c *wsClient) {
	delete(h.clients, c)
}

// sendTo queues msg for one browser, dropping the browser if its queue is
// full. The hub never waits on a browser: one stalled tab must not freeze the
// dashboard for everyone else, or the node readers behind the hub.
func (h *hub) sendTo(c *wsClient, msg []byte) {
	select {
	case c.out <- msg:
	default:
		h.s.log.Warn("dropping slow dashboard client")
		delete(h.clients, c)
		c.cancel()
	}
}

func (h *hub) broadcast(msg []byte) {
	if msg == nil {
		return
	}
	for c := range h.clients {
		h.sendTo(c, msg)
	}
}

func (h *hub) event(kind string, node protocol.NodeID, detail string) {
	b, err := json.Marshal(Event{Type: TypeEvent, AtUnixMS: h.now().UnixMilli(), Kind: kind, Node: node, Detail: detail})
	if err != nil {
		h.s.log.Error("encode event", "err", err)
		return
	}
	h.broadcast(b)
}

func (h *hub) snapshot() Snapshot {
	now := h.now()
	s := Snapshot{
		Type:     TypeSnapshot,
		AtUnixMS: now.UnixMilli(),
		Nodes:    make([]NodeView, 0, len(h.nodes)),
		Tasks:    make([]TaskView, 0, len(h.tasks)),
	}
	for _, n := range h.nodes {
		v := NodeView{
			ID:         n.id,
			Role:       n.tel.Role,
			State:      n.tel.State,
			Term:       n.tel.Term,
			Leader:     n.tel.Leader,
			Degraded:   n.tel.Degraded,
			Connected:  n.link != nil,
			LastSeenMS: now.Sub(n.lastSeen).Milliseconds(),
			Dropped:    n.tel.Dropped,
			LedgerSize: n.tel.LedgerSize,
			Peers:      n.tel.Peers,
			Scores:     n.tel.Scores,
		}
		// The browser does peers.length and Object.entries(scores); null
		// would throw.
		if v.Peers == nil {
			v.Peers = []protocol.MemberRecord{}
		}
		if v.Scores == nil {
			v.Scores = map[protocol.NodeAddress]float64{}
		}
		s.Nodes = append(s.Nodes, v)
	}
	sort.Slice(s.Nodes, func(i, j int) bool { return s.Nodes[i].ID < s.Nodes[j].ID })
	for _, t := range h.tasks {
		s.Tasks = append(s.Tasks, t.view)
	}
	return s
}

func (h *hub) snapshotBytes() []byte {
	b, err := json.Marshal(h.snapshot())
	if err != nil {
		h.s.log.Error("encode snapshot", "err", err)
		return nil
	}
	return b
}
