package helps

import "testing"

// v8's FilterSSEUsageMetadata renames the key in place and leaves the
// upstream cpaUsageMetadata as a duplicate key; the agy build overwrote it.
func TestFilterAgySSEUsageMetadataOverwritesUpstreamCPAUsage(t *testing.T) {
	in := []byte(`data: {"response":{"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":0},"cpaUsageMetadata":{"promptTokenCount":999}}}`)
	want := `data: {"response":{"cpaUsageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":0}}}`
	if got := string(FilterAgySSEUsageMetadata(in)); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	terminal := []byte(`data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1}}}`)
	if got := string(FilterAgySSEUsageMetadata(terminal)); got != string(terminal) {
		t.Fatalf("terminal chunk changed: %s", got)
	}
}
