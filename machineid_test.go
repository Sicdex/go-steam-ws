package steam

import (
	"bytes"
	"regexp"
	"testing"
)

func TestGenerateMachineID_Format(t *testing.T) {
	id := generateMachineID("qubbt09328")

	// Opens with a None-type node named "MessageObject".
	prefix := append([]byte{0x00}, []byte("MessageObject\x00")...)
	if !bytes.HasPrefix(id, prefix) {
		t.Fatalf("machine_id does not start with MessageObject node: %q", id)
	}
	// Closes with two End (0x08) bytes: end-of-children + end-of-document.
	if n := len(id); n < 2 || id[n-1] != 0x08 || id[n-2] != 0x08 {
		t.Fatalf("machine_id must end with 0x08 0x08, got %q", id)
	}
	// Each of the three keys appears as a String (0x01) entry with a 40-char
	// lowercase-hex SHA-1 value.
	for _, key := range []string{"BB3", "FF2", "3B3"} {
		re := regexp.MustCompile("\x01" + key + "\x00[0-9a-f]{40}\x00")
		if !re.Match(id) {
			t.Errorf("machine_id missing well-formed %s entry: %q", key, id)
		}
	}
}

func TestGenerateMachineID_StableAndUnique(t *testing.T) {
	a1 := generateMachineID("alice")
	a2 := generateMachineID("alice")
	b := generateMachineID("bob")

	if !bytes.Equal(a1, a2) {
		t.Error("machine_id must be deterministic for the same username")
	}
	if bytes.Equal(a1, b) {
		t.Error("machine_id must differ between usernames")
	}

	// The three per-account values must differ from each other (as they do on
	// real hardware: distinct GUID / MAC / disk hashes).
	vals := regexp.MustCompile("[0-9a-f]{40}").FindAll(a1, -1)
	if len(vals) != 3 {
		t.Fatalf("expected 3 hash values, got %d", len(vals))
	}
	if bytes.Equal(vals[0], vals[1]) || bytes.Equal(vals[1], vals[2]) || bytes.Equal(vals[0], vals[2]) {
		t.Errorf("BB3/FF2/3B3 must be distinct, got %s", vals)
	}
}

func TestMachineName(t *testing.T) {
	if got := machineName("alice"); got != machineName("alice") {
		t.Error("machineName must be deterministic")
	}
	if machineName("alice") == machineName("bob") {
		t.Error("machineName must differ between usernames")
	}
	if got := machineName("qubbt09328"); !regexp.MustCompile(`^DESKTOP-[A-Z0-9]{7}$`).MatchString(got) {
		t.Errorf("machineName %q is not DESKTOP-XXXXXXX", got)
	}
}
