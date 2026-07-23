package report

import "testing"

func TestSignedMarkerRoundTrip(t *testing.T) {
	key := []byte("test-key")
	body := "some report\n" + SignedMarker(42, 3, key) + "\nmore text"

	pid, mr, ok := ParseSignedMarker(body, key)
	if !ok {
		t.Fatal("ParseSignedMarker ok = false, want true")
	}
	if pid != 42 || mr != 3 {
		t.Fatalf("parsed (pipeline=%d, mr=%d), want (42, 3)", pid, mr)
	}
}

func TestSignedMarkerRejectsTamper(t *testing.T) {
	key := []byte("test-key")
	good := SignedMarker(42, 3, key)
	tampered := replaceFirst(good, "p=42", "p=99")

	if _, _, ok := ParseSignedMarker(tampered, key); ok {
		t.Fatal("ParseSignedMarker accepted a tampered marker")
	}
}

func TestSignedMarkerRejectsWrongKey(t *testing.T) {
	body := SignedMarker(42, 3, []byte("real-key"))
	if _, _, ok := ParseSignedMarker(body, []byte("other-key")); ok {
		t.Fatal("ParseSignedMarker accepted a marker signed with a different key")
	}
}

func TestParseSignedMarkerNoMarker(t *testing.T) {
	if _, _, ok := ParseSignedMarker("no marker here", []byte("k")); ok {
		t.Fatal("ParseSignedMarker ok = true for body without a marker")
	}
}

func TestMarkerPrefixMatchesBothForms(t *testing.T) {
	if !contains(Marker, MarkerPrefix) {
		t.Errorf("MarkerPrefix %q not in Marker %q", MarkerPrefix, Marker)
	}
	if !contains(SignedMarker(1, 1, []byte("k")), MarkerPrefix) {
		t.Error("MarkerPrefix not in SignedMarker output")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
