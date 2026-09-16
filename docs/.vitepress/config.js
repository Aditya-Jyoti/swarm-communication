import { defineConfig } from 'vitepress'

// Sidebar groups are extended dynamically by the `doc-educator` agent after every
// Concept Discovery pass. Keep groups ordered from "how this system works" toward
// "the foundations it stands on", so the site reads as a curriculum rather than a
// reference dump.
export default defineConfig({
  title: 'swarm-net',
  description:
    'A self-healing peer-to-peer swarm network in Go — built in the open, documented as a curriculum.',
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
          { text: 'Repository Layout', link: '/architecture/repo-layout' }
        ]
      },
      {
        text: 'Concepts — Networking & OS Internals',
        collapsed: false,
        items: [
          { text: 'TCP Sockets & The Kernel', link: '/concepts/tcp-sockets-and-the-kernel' },
          { text: 'Stream Framing', link: '/concepts/stream-framing' },
          { text: 'epoll, select & Non-Blocking I/O', link: '/concepts/nonblocking-io-and-epoll' },
          { text: 'Docker Bridge Networking', link: '/concepts/docker-bridge-networking' }
        ]
      },
      {
        text: 'Concepts — Go Runtime & Concurrency',
        collapsed: false,
        items: [
          { text: 'The GMP Scheduler', link: '/concepts/go-scheduler-gmp' },
          { text: 'The Netpoller', link: '/concepts/go-netpoller' },
          { text: 'CSP, Channels & The Memory Model', link: '/concepts/csp-channels-and-memory-model' },
          { text: 'Context & Cancellation Propagation', link: '/concepts/context-cancellation' }
        ]
      },
      {
        text: 'Concepts — Distributed Systems Theory',
        collapsed: false,
        items: []
      }
    ],

    outline: { level: [2, 3], label: 'On this page' },
    search: { provider: 'local' },
    socialLinks: [],
    footer: {
      message: 'Built as a teaching artifact. Every page cites the code it explains.',
      copyright: 'swarm-net'
    }
  }
})
