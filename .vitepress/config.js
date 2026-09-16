import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import mathjax3 from 'markdown-it-mathjax3'

export default withMermaid(
  defineConfig({
    title: 'Swarm Net',
    description: 'Self-healing P2P swarm network in Go',
    lang: 'en-US',

    lastUpdated: true,
    cleanUrls: true,

    markdown: {
      config: (md) => {
        md.use(mathjax3)
      }
    },

    sidebar: {
      '/': [
        {
          text: 'Start Here',
          items: [
            { text: 'Overview', link: '/architecture/overview' },
            { text: 'Repository Layout', link: '/architecture/repo-layout' }
          ]
        },
        {
          text: 'Concepts',
          items: [
            { text: 'TCP Sockets & the Kernel', link: '/concepts/tcp-sockets-and-the-kernel' },
            { text: 'Stream Framing', link: '/concepts/stream-framing' },
            { text: 'Nonblocking I/O & epoll', link: '/concepts/nonblocking-io-and-epoll' },
            { text: 'Docker Bridge Networking', link: '/concepts/docker-bridge-networking' },
            { text: 'Go Scheduler (GMP)', link: '/concepts/go-scheduler-gmp' },
            { text: 'Go Netpoller', link: '/concepts/go-netpoller' },
            { text: 'CSP Channels & Memory Model', link: '/concepts/csp-channels-and-memory-model' },
            { text: 'Context Cancellation', link: '/concepts/context-cancellation' },
            { text: 'Wire Protocol Design', link: '/concepts/wire-protocol-design' },
            { text: 'I/O Reader/Writer Contracts', link: '/concepts/io-reader-writer-contracts' },
            { text: 'Error Wrapping & Classification', link: '/concepts/error-wrapping-and-classification' },
            { text: 'Interface Polymorphism', link: '/concepts/interface-polymorphism' },
            { text: 'Monotonic vs Wall Clocks', link: '/concepts/monotonic-vs-wall-clocks' },
            { text: 'Latency as a Statistic', link: '/concepts/latency-as-a-statistic' }
          ]
        }
      ]
    },

    themeConfig: {
      search: {
        provider: 'local'
      },
      nav: [
        { text: 'Home', link: '/' },
        { text: 'Architecture', link: '/architecture/overview' },
        { text: 'Concepts', link: '/concepts/tcp-sockets-and-the-kernel' }
      ]
    },

    vite: {
      ssr: {
        noExternal: ['vitepress-plugin-mermaid']
      }
    }
  })
)
