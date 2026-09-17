package protocol

// NodeID is a stable, human-meaningful name for a node ("node-7", "control-center").
// It is chosen at start-up, carried in every envelope, and never changes for the
// lifetime of a process.
//
// It is deliberately NOT an address. A node that is restarted by the chaos controls
// keeps its NodeID and almost certainly gets a new IP, and membership logic must
// track the former rather than the latter.
type NodeID string

// NodeAddress is a dialable "host:port" for a peer, where host is normally a Docker
// service name resolved by the embedded DNS resolver at 127.0.0.11.
//
// A NodeAddress is resolved at dial time and never cached as an IP. Docker recycles
// container IPs, so an address cached before a chaos kill can resolve to a different,
// live container afterwards — a failure that is silent and therefore worse than an
// outright error. See docs/concepts/docker-bridge-networking.md.
type NodeAddress string
