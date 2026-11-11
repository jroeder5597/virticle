package cluster

// KnownHostsFile is the known_hosts file the manager records trusted host keys
// in; "node prepare" records the key of a host it signs in to there as well,
// so the node can be added without a second trust decision.
func (m *Manager) KnownHostsFile() string { return m.knownHosts }
