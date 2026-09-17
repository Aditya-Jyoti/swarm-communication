import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import mathjax3 from 'markdown-it-mathjax3'

// Sidebar groups are extended dynamically by the `doc-educator` agent after every
// Concept Discovery pass. Keep groups ordered from "how this system works" toward
// "the foundations it stands on", so the site reads as a curriculum rather than a
// reference dump.
export default withMermaid(defineConfig({
  title: 'swarm-net',
  description:
    'A self-healing peer-to-peer swarm network in Go -- built in the open, documented as a curriculum.',
  lang: 'en-US',
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
          { text: 'Repository Layout', link: '/architecture/repo-layout' },
          { text: 'Why Not Consensus', link: '/architecture/why-not-consensus' },
          { text: 'The Mesh and the Handshake', link: '/architecture/mesh-and-handshake' }
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
          { text: 'TCP Teardown & Half-Open Sockets', link: '/concepts/tcp-teardown-and-half-open-sockets' },
          { text: 'Deadlines & I/O Timeouts', link: '/concepts/deadlines-and-io-timeouts' },
          { text: 'Backpressure & Bounded Queues', link: '/concepts/backpressure-and-bounded-queues' },
          { text: 'Backoff & Connection Storms', link: '/concepts/backoff-and-connection-storms' }
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
          { text: 'Monotonic vs Wall Clocks', link: '/concepts/monotonic-vs-wall-clocks' }
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
          { text: 'Split-Brain & Quorum', link: '/concepts/split-brain-and-quorum' },
          { text: 'Idempotence & Hysteresis', link: '/concepts/idempotence-and-hysteresis' }
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
  markdown: {
    config: (md) => {
      md.use(mathjax3)
    }
  },

  // Mermaid replaces ASCII art entirely, per CLAUDE.md 4.2.
  mermaid: {
    theme: 'base',
    securityLevel: 'strict'
  }
}))
