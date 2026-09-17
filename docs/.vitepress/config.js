import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

// GitHub Pages serves this repo as a *project* site under
// https://<owner>.github.io/swarm-communication/, not at the domain root. VitePress
// emits root-absolute asset URLs (/assets/app.js), so without a matching `base` every
// script 404s and the page stays blank. DOCS_BASE overrides it (e.g. DOCS_BASE=/ for a
// custom domain or a user site); the value is normalised to "/x/" form.
function resolveBase(raw) {
  const trimmed = (raw ?? '/swarm-communication/').trim().replace(/^\/+|\/+$/g, '')
  return trimmed ? `/${trimmed}/` : '/'
}

// Sidebar groups are extended dynamically by the `doc-educator` agent after every
// Concept Discovery pass. Keep groups ordered from "how this system works" toward
// "the foundations it stands on", so the site reads as a curriculum rather than a
// reference dump.
export default withMermaid(defineConfig({
  title: 'swarm-net',
  description:
    'A self-healing peer-to-peer swarm network in Go -- built in the open, documented as a curriculum.',
  lang: 'en-US',
  base: resolveBase(process.env.DOCS_BASE),
  cleanUrls: true,
  lastUpdated: true,

  themeConfig: {
    nav: [
      { text: 'Architecture', link: '/architecture/overview' },
      { text: 'Concepts', link: '/concepts/tcp-sockets-and-the-kernel' },
      { text: 'Worklog', link: '/WORKLOG' }
    ],

    sidebar: [
      {
        text: 'Start Here',
        collapsed: false,
        items: [
          { text: 'Introduction', link: '/' },
          { text: 'Engineering Worklog', link: '/WORKLOG' }
        ]
      },
      {
        text: 'Architecture',
        collapsed: false,
        items: [
          { text: 'System Overview', link: '/architecture/overview' },
          { text: 'The Mesh and the Handshake', link: '/architecture/mesh-and-handshake' },
          { text: 'Failure Detection & Failover', link: '/architecture/failure-detection-and-failover' },
          { text: 'Replication & Tasks', link: '/architecture/replication-and-tasks' },
          { text: 'The Control Center', link: '/architecture/control-center' },
          { text: 'Drone Simulation', link: '/architecture/drone-simulation' },
          { text: 'Running the Swarm', link: '/architecture/running-the-swarm' },
          { text: 'Blender Scene Tooling', link: '/architecture/blender-scene' },
          { text: 'Why Not Consensus', link: '/architecture/why-not-consensus' },
          { text: 'Case Study: Convergence Bugs', link: '/architecture/convergence-debugging' },
          { text: 'Repository Layout', link: '/architecture/repo-layout' }
        ]
      },
      {
        text: 'Concepts -- Networking & OS Internals',
        collapsed: false,
        items: [
          { text: 'TCP Sockets & The Kernel', link: '/concepts/tcp-sockets-and-the-kernel' },
          { text: 'Stream Framing', link: '/concepts/stream-framing' },
          { text: 'io.Reader, io.Writer & ReadFull', link: '/concepts/io-reader-writer-contracts' },
          { text: 'Wire Protocol Design', link: '/concepts/wire-protocol-design' },
          { text: 'epoll, select & Non-Blocking I/O', link: '/concepts/nonblocking-io-and-epoll' },
          { text: 'Docker Bridge Networking', link: '/concepts/docker-bridge-networking' },
          { text: 'Network Emulation: Geometry to Latency', link: '/concepts/network-emulation' },
          { text: 'TCP Teardown & Half-Open Sockets', link: '/concepts/tcp-teardown-and-half-open-sockets' },
          { text: 'Deadlines & I/O Timeouts', link: '/concepts/deadlines-and-io-timeouts' },
          { text: 'Backpressure & Bounded Queues', link: '/concepts/backpressure-and-bounded-queues' },
          { text: 'Backoff & Connection Storms', link: '/concepts/backoff-and-connection-storms' },
          { text: 'WebSocket Framing & The HTTP Upgrade', link: '/concepts/websocket-framing-and-upgrade' },
          { text: 'Graceful Shutdown & Teardown Ordering', link: '/concepts/graceful-shutdown-and-teardown-ordering' },
          { text: 'Container Images & PID 1', link: '/concepts/container-images-and-pid-1' }
        ]
      },
      {
        text: 'Concepts -- Go Runtime & Concurrency',
        collapsed: false,
        items: [
          { text: 'The GMP Scheduler', link: '/concepts/go-scheduler-gmp' },
          { text: 'The Netpoller', link: '/concepts/go-netpoller' },
          { text: 'CSP, Channels & The Memory Model', link: '/concepts/csp-channels-and-memory-model' },
          { text: 'Context & Cancellation Propagation', link: '/concepts/context-cancellation' },
          { text: 'Interface Polymorphism', link: '/concepts/interface-polymorphism' },
          { text: 'Error Wrapping & Classification', link: '/concepts/error-wrapping-and-classification' },
          { text: 'Monotonic vs Wall Clocks', link: '/concepts/monotonic-vs-wall-clocks' },
          { text: 'Heartbeat Intervals, Jitter & Timers', link: '/concepts/heartbeat-intervals-and-timers' }
        ]
      },
      {
        text: 'Concepts -- Distributed Systems Theory',
        collapsed: false,
        items: [
          { text: 'Latency as a Statistic', link: '/concepts/latency-as-a-statistic' },
          { text: 'Failure Detectors', link: '/concepts/failure-detectors' },
          { text: 'Gossip & Anti-Entropy', link: '/concepts/gossip-and-anti-entropy' },
          { text: 'Monotonic Merge & Incarnation', link: '/concepts/monotonic-merge-and-incarnation' },
          { text: 'Logical Clocks: Terms & Sequence Numbers', link: '/concepts/logical-clocks' },
          { text: 'Split-Brain & Quorum', link: '/concepts/split-brain-and-quorum' },
          { text: 'Idempotence & Hysteresis', link: '/concepts/idempotence-and-hysteresis' },
          { text: 'At-Least-Once Delivery & Idempotent Re-issue', link: '/concepts/at-least-once-delivery' },
          { text: 'Cooperative vs Uncooperative Failure Injection', link: '/concepts/cooperative-vs-uncooperative-failure-injection' }
        ]
      },
      {
        text: 'Concepts -- Visualisation & Algorithms',
        collapsed: false,
        items: [
          { text: '3D Perspective Projection & Depth Sorting', link: '/concepts/3d-perspective-projection-and-depth-sorting' },
          { text: 'Hash Mixing: Why FNV Needs a Finalizer', link: '/concepts/hash-mixing-and-finalizers' }
        ]
      }
    ],

    outline: { level: [2, 3], label: 'On this page' },
    search: { provider: 'local' },
    socialLinks: [],
    footer: {
      message: 'Built as a teaching artifact. Every page cites the code it explains.',
      copyright: 'swarm-net'
    }
  },

  // LaTeX: $inline$ and $$block$$, per CLAUDE.md 4.3.
  // VitePress's built-in `math` switch drives markdown-it-mathjax3 (still a devDependency)
  // AND registers the <mjx-*> tags as Vue custom elements. Calling md.use(mathjax3) by
  // hand skips the latter, so Vue treats <mjx-container> as an unresolved component and
  // every math page logs "Hydration completed but contains mismatches".
  markdown: {
    math: true
  },

  // Mermaid replaces ASCII art entirely, per CLAUDE.md 4.2.
  mermaid: {
    theme: 'base',
    securityLevel: 'strict'
  }
}))
