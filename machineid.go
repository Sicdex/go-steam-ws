package steam

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// generateMachineID builds the Steam "machine_id" blob a real desktop client
// sends with logon: a binary-serialized Valve KeyValues "MessageObject" with
// three SHA-1 hex values keyed BB3 (machine GUID), FF2 (primary MAC) and 3B3
// (boot-disk serial). A genuine client derives those three from real hardware;
// for a headless account pool we derive them deterministically from the account
// name instead, so each account always presents the SAME, UNIQUE machine —
// stable across restarts, distinct per account, and needing no on-disk identity
// store. Only the source of the three values differs from SteamKit2; the wire
// shape is byte-for-byte what Steam expects.
//
// Wire format (Valve binary KeyValues — type bytes: 0=node, 1=string, 8=end):
//
//	00 "MessageObject\0"
//	  01 "BB3\0" "<40 hex>\0"
//	  01 "FF2\0" "<40 hex>\0"
//	  01 "3B3\0" "<40 hex>\0"
//	08            (end of MessageObject's children)
//	08            (end of document)
func generateMachineID(username string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0) // KeyValues type None — opens the "MessageObject" node
	writeKVCString(&buf, "MessageObject")
	writeMachineIDValue(&buf, "BB3", username)
	writeMachineIDValue(&buf, "FF2", username)
	writeMachineIDValue(&buf, "3B3", username)
	buf.WriteByte(8) // end of the node's children
	buf.WriteByte(8) // end of document
	return buf.Bytes()
}

// writeMachineIDValue appends one `01 <key>\0 <value>\0` String entry, where the
// value is a SHA-1 hex derived from the key + account name. The per-key salt
// makes BB3/FF2/3B3 distinct for a single account (as they are on real
// hardware), while staying fully deterministic.
func writeMachineIDValue(buf *bytes.Buffer, key, username string) {
	buf.WriteByte(1) // KeyValues type String
	writeKVCString(buf, key)
	sum := sha1.Sum([]byte("SteamMachineID|" + key + "|" + username))
	writeKVCString(buf, hex.EncodeToString(sum[:]))
}

func writeKVCString(buf *bytes.Buffer, s string) {
	buf.WriteString(s)
	buf.WriteByte(0)
}

// steamPrivateIPMagic is the constant the Steam client XORs the LAN IP with
// before putting it in (deprecated_)obfuscated_private_ip — Steam's trivial
// "obfuscation". Mirrors SteamKit2's NetHelpers.MagicNumber.
const steamPrivateIPMagic uint32 = 0xBAADF00D

// obfuscatedPrivateIP returns what a real client reports as its private LAN
// address: the IPv4 XOR'd with the magic number above. A genuine desktop sends
// its real 192.168/10.x address; for a headless pool we synthesize a stable,
// plausible one per account so the field is populated like a real client (Steam
// never validates it against the connection).
func obfuscatedPrivateIP(username string) uint32 {
	sum := sha1.Sum([]byte("SteamPrivateIP|" + username))
	// 192.168.x.y — a common home-LAN range; force the host octet to 1..255.
	ip := uint32(192)<<24 | uint32(168)<<16 | uint32(sum[0])<<8 | uint32(sum[1]|1)
	return ip ^ steamPrivateIPMagic
}

// machineName derives a stable, unique Windows-style computer name
// ("DESKTOP-XXXXXXX") from the account name, matching the Windows
// client_os_type we report at logon.
func machineName(username string) string {
	sum := sha1.Sum([]byte("SteamMachineName|" + username))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var sb strings.Builder
	sb.Grow(len("DESKTOP-") + 7)
	sb.WriteString("DESKTOP-")
	for i := 0; i < 7; i++ {
		sb.WriteByte(alphabet[int(sum[i])%len(alphabet)])
	}
	return sb.String()
}
