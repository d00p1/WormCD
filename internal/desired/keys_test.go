package desired

import "testing"

func TestParseKeyRoundTrip(t *testing.T) {
	for _, key := range []Key{
		{Kind: KindService, Name: "wormcd_web_prod_web"},
		{Kind: KindConfig, Name: "nginx_abc123"},
	} {
		got, ok := ParseKey(key.String())
		if !ok || got != key {
			t.Fatalf("ParseKey(%q) = %+v, %v; want %+v", key.String(), got, ok, key)
		}
	}
}

func TestParseKeyRejectsUnknownKinds(t *testing.T) {
	for _, s := range []string{"web", "Deployment/web", ""} {
		if _, ok := ParseKey(s); ok {
			t.Errorf("ParseKey(%q) accepted a key", s)
		}
	}
}
