package aiops

import (
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// detailedLLM is a model.DetailedLLM whose Chat must not be used when
// ChatDetailed is available.
type detailedLLM struct {
	res   model.ChatResult
	chats int
}

func (d *detailedLLM) Chat([]model.Message, *model.ChatOptions) (string, error) {
	d.chats++
	return d.res.Reply, nil
}
func (d *detailedLLM) ChatDetailed([]model.Message, *model.ChatOptions) (model.ChatResult, error) {
	return d.res, nil
}
func (d *detailedLLM) Available() bool   { return true }
func (d *detailedLLM) ModelName() string { return "detailed" }
func (d *detailedLLM) Backend() string   { return "detailed" }

func TestSummarizeWithLLM_CarriesMitigation(t *testing.T) {
	m := &model.Mitigation{Source: "S", Plan: "roll back", Result: model.MitigationOK}
	llm := &detailedLLM{res: model.ChatResult{Reply: "## Root cause\nbad deploy", Mitigation: m}}
	alert := Alert{Labels: map[string]string{"alertname": "a", "host": "h"}, Status: "firing"}

	res, err := SummarizeWithLLM(llm, alert, nil, "")
	if err != nil {
		t.Fatalf("SummarizeWithLLM: %v", err)
	}
	if llm.chats != 0 {
		t.Errorf("plain Chat called %d times; ChatDetailed should be used", llm.chats)
	}
	if res.Mitigation != m {
		t.Errorf("Mitigation = %+v, want the backend's", res.Mitigation)
	}
	if res.Summary != "## Root cause\nbad deploy" {
		t.Errorf("Summary = %q", res.Summary)
	}
}

func TestSummarizeWithLLM_PlainLLM_NoMitigation(t *testing.T) {
	llm := &fakeLLM{reply: `{"summary":"x","confidence":"high","escalate":false,"reason":"r"}`}
	res, err := SummarizeWithLLM(llm, Alert{Labels: map[string]string{"alertname": "a", "host": "h"}}, nil, "")
	if err != nil {
		t.Fatalf("SummarizeWithLLM: %v", err)
	}
	if res.Mitigation != nil {
		t.Errorf("Mitigation = %+v, want nil for a plain LLM", res.Mitigation)
	}
}
