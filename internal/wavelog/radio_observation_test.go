package wavelog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"waveloggate/internal/config"
)

func TestV2RadioUpdateCarriesObservationIdentity(t *testing.T) {
	var payload map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/radio" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer wl2_test" {
			t.Fatalf("missing bearer token")
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(&config.Profile{WavelogURL: server.URL, WavelogKey: "wl2_test", WavelogRadioname: "RGO One"}, "test")
	observedAt := time.Date(2026, 10, 3, 2, 50, 0, 123000000, time.UTC)
	err := client.UpdateRadioStatus(RadioData{Frequency: 14042300, Mode: "USB", SourceSessionID: "session-a", SourceSequence: 42, SourceObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	if payload["source_session_id"] != "session-a" || payload["source_sequence"] != float64(42) || payload["source_observed_at"] != observedAt.Format(time.RFC3339Nano) {
		t.Fatalf("missing observation correlation: %#v", payload)
	}
}

func TestRadioUpdateReportsServerDeliveryFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := New(&config.Profile{WavelogURL: server.URL, WavelogKey: "wl2_test", WavelogRadioname: "RGO One"}, "test")
	if err := client.UpdateRadioStatus(RadioData{Frequency: 14042300, Mode: "USB"}); err == nil {
		t.Fatal("non-2xx radio delivery must fail")
	}
}

func TestV1RadioUpdateOmitsObservationExtension(t *testing.T) {
	var payload map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/radio" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(&config.Profile{WavelogURL: server.URL, WavelogKey: "legacy-key", WavelogRadioname: "RGO One"}, "test")
	err := client.UpdateRadioStatus(RadioData{
		Frequency:        14042300,
		Mode:             "USB",
		SourceSessionID:  "session-a",
		SourceSequence:   42,
		SourceObservedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"source_session_id", "source_sequence", "source_observed_at"} {
		if _, present := payload[field]; present {
			t.Fatalf("legacy v1 payload gained %s: %#v", field, payload)
		}
	}
	if payload["key"] != "legacy-key" {
		t.Fatalf("legacy key handling changed: %#v", payload)
	}
}
