package wire

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// ProtocolVersion is the current geminus wire protocol version spoken by
	// this client.
	ProtocolVersion = 1

	// Bridge capability identifiers advertised in a signed NodeCard.
	CapEd25519       = "ed25519"
	CapMultiPubkey   = "multi_pubkey"
	CapModelMenu     = "model_menu"
	CapProviderModel = "provider_model"

	// MinEd25519BridgeVersion is the minimum geminish version that supports
	// Ed25519 human prompt verification and multi-pubkey configs.
	MinEd25519BridgeVersion = "0.9.2"
)

// CapsLabel binds a node card's protocol version, bridge version, and
// capability list to the peer that published it.
func CapsLabel(peer string) string { return "geminus-node-caps:" + peer }

// CapsText returns the canonical signed representation of a node card's
// protocol version, bridge version, and capability set.
func CapsText(proto int, bridgeVersion string, caps []string) string {
	norm := make([]string, 0, len(caps))
	seen := make(map[string]bool, len(caps))
	for _, c := range caps {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		norm = append(norm, c)
	}
	sort.Strings(norm)
	return fmt.Sprintf("v%d:%s:%s", proto, strings.TrimSpace(bridgeVersion), strings.Join(norm, ","))
}

// CapabilitiesVerified reports whether the node card carries a verified
// protocol/capabilities declaration from the bridge.
func (c NodeCard) CapabilitiesVerified() bool {
	return c.CapsSig != ""
}

// HasCapability reports whether the verified node card advertises cap.
func (c NodeCard) HasCapability(cap string) bool {
	if !c.CapabilitiesVerified() {
		return false
	}
	for _, have := range c.Capabilities {
		if have == cap {
			return true
		}
	}
	return false
}

// CheckBridgeCompatibility returns human-readable warnings when a verified
// bridge node card declares a protocol version or capability set incompatible
// with this geminus client.
func CheckBridgeCompatibility(peer string, card NodeCard, useEd25519 bool) []string {
	if !card.CapabilitiesVerified() {
		return nil
	}
	var warnings []string
	if card.Proto > ProtocolVersion {
		ver := card.BridgeVersion
		if ver == "" {
			ver = "newer"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s runs geminish %s (wire v%d), newer than this geminus (wire v%d); run /update",
			peer, ver, card.Proto, ProtocolVersion,
		))
	}
	if useEd25519 && !card.HasCapability(CapEd25519) {
		ver := card.BridgeVersion
		if ver == "" {
			ver = "unknown"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s (geminish %s) does not advertise %s; Ed25519-signed prompts require geminish >= %s",
			peer, ver, CapEd25519, MinEd25519BridgeVersion,
		))
	}
	return warnings
}

// RefusalHint returns an actionable compatibility hint when a bridge refuses a
// prompt, or "" if the refusal does not look like a version/capability mismatch.
func RefusalHint(peer string, card NodeCard, detail string, useEd25519 bool) string {
	lower := strings.ToLower(detail)
	if !useEd25519 {
		return ""
	}
	if !strings.Contains(lower, "signature") && !strings.Contains(lower, "ed25519") && !strings.Contains(lower, "algorithm") {
		return ""
	}
	if !card.CapabilitiesVerified() {
		return fmt.Sprintf(
			"hint: %s did not advertise capabilities (likely older geminish without %s); upgrade %s's geminish to >= %s",
			peer, CapEd25519, peer, MinEd25519BridgeVersion,
		)
	}
	if !card.HasCapability(CapEd25519) {
		ver := card.BridgeVersion
		if ver == "" {
			ver = "unknown"
		}
		return fmt.Sprintf(
			"hint: %s runs geminish %s without %s support; upgrade %s's geminish to >= %s",
			peer, ver, CapEd25519, peer, MinEd25519BridgeVersion,
		)
	}
	return ""
}
