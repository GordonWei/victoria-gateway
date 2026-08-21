package aiops

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueryRange_Success(t *testing.T) {
	fixture := `{
		"status": "success",
		"data": {
			"resultType": "streams",
			"result": [
				{
					"stream": {"host": "web-01"},
					"values": [
						["1724123400000000000", "Aug 20 10:30:00 web-01 kernel: CPU throttling detected"],
						["1724123401000000000", "Aug 20 10:30:01 web-01 syslog: high load average 8.5"]
					]
				}
			]
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query().Get("query")
		if q != `{host="web-01"}` {
			t.Errorf("unexpected query param: %s", q)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)

	entries, err := client.QueryRange("web-01", start, end, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries count = %d, want 2", len(entries))
	}
	if entries[0].Line != "Aug 20 10:30:00 web-01 kernel: CPU throttling detected" {
		t.Errorf("first line = %q", entries[0].Line)
	}
	if entries[0].Timestamp.UnixNano() != 1724123400000000000 {
		t.Errorf("first timestamp = %d", entries[0].Timestamp.UnixNano())
	}
}

func TestQueryRange_EmptyResult(t *testing.T) {
	fixture := `{
		"status": "success",
		"data": {
			"resultType": "streams",
			"result": []
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)

	entries, err := client.QueryRange("ghost-host", start, end, 10)
	if err != nil {
		t.Fatalf("unexpected error for empty result: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries count = %d, want 0", len(entries))
	}
}

func TestQueryRange_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":"error","message":"internal server error"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)

	_, err := client.QueryRange("web-01", start, end, 10)
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	// error message should contain status code
	if got := err.Error(); !containsSubstring(got, "500") {
		t.Errorf("error = %q, expected to contain \"500\"", got)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
