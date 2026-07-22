package report

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// MarkerPrefix is the stable substring UpsertNote matches to find the bot's
// note (present in both the plain Marker and the signed marker), keeping the
// report idempotent regardless of the signature that follows.
const MarkerPrefix = "<!-- ci-resources-bot"

func markerSignature(pipelineID, mrIID int64, key []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "p=%d;m=%d", pipelineID, mrIID)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignedMarker renders the HMAC-signed marker embedded at the top of the report
// body. It pins the report's pipeline and MR so command replies can trust them.
func SignedMarker(pipelineID, mrIID int64, key []byte) string {
	return fmt.Sprintf("%s p=%d m=%d sig=%s -->",
		MarkerPrefix, pipelineID, mrIID, markerSignature(pipelineID, mrIID, key))
}

var signedMarkerRE = regexp.MustCompile(`<!-- ci-resources-bot p=(\d+) m=(\d+) sig=([0-9a-f]+) -->`)

// ParseSignedMarker finds and verifies a signed marker in body. ok is false when
// no marker is present or its signature does not verify against key (tampered).
func ParseSignedMarker(body string, key []byte) (pipelineID, mrIID int64, ok bool) {
	m := signedMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return 0, 0, false
	}
	pipelineID, _ = strconv.ParseInt(m[1], 10, 64)
	mrIID, _ = strconv.ParseInt(m[2], 10, 64)
	want := markerSignature(pipelineID, mrIID, key)
	if !hmac.Equal([]byte(m[3]), []byte(want)) {
		return 0, 0, false
	}
	return pipelineID, mrIID, true
}
