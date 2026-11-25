package p2p

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// PeerCacheFile is the filename stored inside HERMES_HOME for cached P2P
// endpoints so failover works even when Honcho is unreachable at startup.
const PeerCacheFile = "geminus_peers.json"

// CachedPeer records the last-known transport v2 endpoints for one peer.
type CachedPeer struct {
	Name       string `json:"name"`
	NodeID     string `json:"node_id,omitempty"`
	DirectAddr string `json:"direct_addr,omitempty"`
	RelayAddr  string `json:"relay_addr,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
}

// PeerCache holds cached peer endpoints on disk.
type PeerCache struct {
	Peers map[string]CachedPeer `json:"peers"`
}

// LoadPeerCache reads path, returning an empty cache if the file does not exist.
func LoadPeerCache(path string) (*PeerCache, error) {
	c := &PeerCache{Peers: map[string]CachedPeer{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	if c.Peers == nil {
		c.Peers = map[string]CachedPeer{}
	}
	return c, nil
}

// Upsert records or updates a peer entry with the current timestamp.
func (c *PeerCache) Upsert(p CachedPeer) {
	if c.Peers == nil {
		c.Peers = map[string]CachedPeer{}
	}
	if p.UpdatedAt == 0 {
		p.UpdatedAt = time.Now().Unix()
	}
	c.Peers[p.Name] = p
}

// Save writes the peer cache atomically with 0600 permissions.
func (c *PeerCache) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
