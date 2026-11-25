package p2p

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"time"
)

// DefaultDiscoveryPort is the UDP port used for authenticated LAN peer discovery.
const DefaultDiscoveryPort = 4434

// BeaconPayload is broadcast inside an ObfsPacketConn-encrypted UDP datagram.
type BeaconPayload struct {
	Peer   string `json:"peer"`
	NodeID string `json:"node_id"`
	Port   int    `json:"port"`
	TS     int64  `json:"ts"`
}

// DiscoveredPeer is a peer heard on the local network.
type DiscoveredPeer struct {
	Peer   string
	NodeID string
	Addr   string
}

// StartLANBeacon periodically broadcasts an authenticated LAN announcement for
// peerName until ctx is canceled.
func StartLANBeacon(ctx context.Context, secret []byte, peerName, nodeID string, servicePort, discoveryPort int) error {
	if discoveryPort == 0 {
		discoveryPort = DefaultDiscoveryPort
	}
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return err
	}
	obfs, err := NewObfsPacketConn(pc, secret, 0)
	if err != nil {
		_ = pc.Close()
		return err
	}
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: discoveryPort}
	send := func() {
		raw, err := json.Marshal(BeaconPayload{
			Peer:   peerName,
			NodeID: nodeID,
			Port:   servicePort,
			TS:     time.Now().Unix(),
		})
		if err != nil {
			return
		}
		_, _ = obfs.WriteTo(raw, dst)
	}
	go func() {
		defer obfs.Close()
		send()
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				send()
			}
		}
	}()
	return nil
}

// DiscoverLAN listens for authenticated LAN beacons for up to timeout.
func DiscoverLAN(ctx context.Context, secret []byte, discoveryPort int, timeout time.Duration) ([]DiscoveredPeer, error) {
	if discoveryPort == 0 {
		discoveryPort = DefaultDiscoveryPort
	}
	pc, err := net.ListenPacket("udp4", ":"+strconv.Itoa(discoveryPort))
	if err != nil {
		return nil, err
	}
	obfs, err := NewObfsPacketConn(pc, secret, 0)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	defer obfs.Close()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = obfs.SetReadDeadline(deadline)

	found := map[string]DiscoveredPeer{}
	buf := make([]byte, 2048)
	for {
		n, addr, err := obfs.ReadFrom(buf)
		if err != nil {
			break
		}
		var b BeaconPayload
		if err := json.Unmarshal(buf[:n], &b); err != nil || b.Peer == "" || b.Port <= 0 {
			continue
		}
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		found[b.Peer] = DiscoveredPeer{
			Peer:   b.Peer,
			NodeID: b.NodeID,
			Addr:   net.JoinHostPort(udpAddr.IP.String(), strconv.Itoa(b.Port)),
		}
	}
	out := make([]DiscoveredPeer, 0, len(found))
	for _, p := range found {
		out = append(out, p)
	}
	return out, nil
}
