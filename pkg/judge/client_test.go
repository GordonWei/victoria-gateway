package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJudgeEscalation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		var req systemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != DefaultModel {
			t.Errorf("model = %q, want %q", req.Model, DefaultModel)
		}
		if _, ok := req.Questions[severityQuestionID]; !ok {
			t.Errorf("request missing %q question", severityQuestionID)
		}
		if _, ok := req.Questions[escalationQuestionID]; !ok {
			t.Errorf("request missing %q question", escalationQuestionID)
		}

		resp := systemOneResponse{
			Model: DefaultModel,
			Answers: map[string]Answer{
				severityQuestionID: {
					Score:         2.3,
					Confidence:    0.81,
					Probabilities: map[string]float64{"0": 0.0, "1": 0.1, "2": 0.5, "3": 0.4},
				},
				escalationQuestionID: {
					Noul: 0.72,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL)
	got, err := c.JudgeEscalation(context.Background(), "InstanceDown", "主機無法連線，log 內容不足以判斷根因")
	if err != nil {
		t.Fatalf("JudgeEscalation: %v", err)
	}
	if got.Severity != 2.3 || got.SeverityConfidence != 0.81 || got.EscalateProbability != 0.72 {
		t.Errorf("got %+v", got)
	}
}

func TestJudgeEscalationMissingQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(systemOneResponse{
			Answers: map[string]Answer{
				severityQuestionID: {Score: 1.0},
				// escalationQuestionID deliberately omitted
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL)
	_, err := c.JudgeEscalation(context.Background(), "X", "Y")
	if err == nil || !strings.Contains(err.Error(), escalationQuestionID) {
		t.Fatalf("expected error mentioning %q, got %v", escalationQuestionID, err)
	}
}

func TestAskErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("bad-key", srv.URL)
	_, err := c.Ask(context.Background(), "state", map[string]Question{
		"q": {Type: TypeNoul, Instructions: "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected error mentioning 401, got %v", err)
	}
}

func TestAskNoQuestions(t *testing.T) {
	c := NewClient("key")
	_, err := c.Ask(context.Background(), "state", nil)
	if err == nil {
		t.Fatal("expected error for empty questions map")
	}
}
