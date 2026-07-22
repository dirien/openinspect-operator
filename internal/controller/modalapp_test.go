package controller

import "testing"

// Separator-only framing would let a value containing the separator byte
// alias a different key/value split; length-prefixed framing must not.
func TestHashModalSecretsFramingResistsAliasing(t *testing.T) {
	a := map[string]map[string]string{"s": {"A": "B\x00C\x00D"}}
	b := map[string]map[string]string{"s": {"A": "B", "C": "D"}}
	if hashModalSecrets(a, "key") == hashModalSecrets(b, "key") {
		t.Error("distinct secret sets hash identically: framing is aliasable")
	}
	if hashModalSecrets(a, "key") != hashModalSecrets(a, "key") {
		t.Error("hash is not deterministic")
	}
	if hashModalSecrets(a, "key") == hashModalSecrets(a, "other-key") {
		t.Error("hash must be keyed")
	}
}
