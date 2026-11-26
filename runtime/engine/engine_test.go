package engine

import "testing"

// baseInfo is a fully-populated ModelInfo: every field distinct, so
// changing any one of them must change the fingerprint.
func baseInfo() ModelInfo {
	return ModelInfo{
		ID:            "jarvis-brain",
		ContextTokens: 131072,
		KVType:        "q8_0",
		Build:         "b4321",
		ModelFile:     "/opt/jarvis-engine/models/jarvis-brain.gguf",
		ModelBytes:    15300000000,
	}
}

// TestFingerprintStable checks that the same ModelInfo always gives the
// same fingerprint.
func TestFingerprintStable(t *testing.T) {
	a, b := baseInfo(), baseInfo()
	if a.Fingerprint() != b.Fingerprint() {
		t.Errorf("same info, different fingerprints: %s vs %s", a.Fingerprint(), b.Fingerprint())
	}
	if got := a.Fingerprint(); len(got) != 16 {
		t.Errorf("fingerprint is %d hex chars, want 16: %q", len(got), got)
	}
}

// TestFingerprintChanges checks that changing any one field changes the
// fingerprint. It loops over the six fields, perturbing exactly one in
// each variant.
func TestFingerprintChanges(t *testing.T) {
	base := baseInfo().Fingerprint()
	variants := map[string]ModelInfo{
		"ID":            {ID: "other-model", ContextTokens: 131072, KVType: "q8_0", Build: "b4321", ModelFile: "/opt/jarvis-engine/models/jarvis-brain.gguf", ModelBytes: 15300000000},
		"ContextTokens": {ID: "jarvis-brain", ContextTokens: 32768, KVType: "q8_0", Build: "b4321", ModelFile: "/opt/jarvis-engine/models/jarvis-brain.gguf", ModelBytes: 15300000000},
		"KVType":        {ID: "jarvis-brain", ContextTokens: 131072, KVType: "f16", Build: "b4321", ModelFile: "/opt/jarvis-engine/models/jarvis-brain.gguf", ModelBytes: 15300000000},
		"Build":         {ID: "jarvis-brain", ContextTokens: 131072, KVType: "q8_0", Build: "b4400", ModelFile: "/opt/jarvis-engine/models/jarvis-brain.gguf", ModelBytes: 15300000000},
		"ModelFile":     {ID: "jarvis-brain", ContextTokens: 131072, KVType: "q8_0", Build: "b4321", ModelFile: "/opt/other/models/jarvis-brain.gguf", ModelBytes: 15300000000},
		"ModelBytes":    {ID: "jarvis-brain", ContextTokens: 131072, KVType: "q8_0", Build: "b4321", ModelFile: "/opt/jarvis-engine/models/jarvis-brain.gguf", ModelBytes: 15300000001},
	}
	seen := map[string]string{} // fingerprint -> field that produced it
	for field, m := range variants {
		fp := m.Fingerprint()
		if fp == base {
			t.Errorf("changing %s did not change the fingerprint: %s", field, fp)
		}
		if prev, dup := seen[fp]; dup {
			t.Errorf("changing %s collides with the %s variant: %s", field, prev, fp)
		}
		seen[fp] = field
	}
	if len(seen) != len(variants) {
		t.Errorf("want %d distinct fingerprints, got %d: %v", len(variants), len(seen), seen)
	}
}
