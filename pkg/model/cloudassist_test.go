package model

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testInvName = "projects/p1/locations/global/investigations/inv-1"
	testRevName = testInvName + "/revisions/rev-1"
	testOpName  = "projects/123/locations/global/operations/op-1"
)

// fakeCloudAssist is an in-memory Gemini Cloud Assist API: create →
// run → operation polled pollsUntilDone times → investigation. Each
// field can be changed before the test's first call to force a failure.
type fakeCloudAssist struct {
	t              *testing.T
	mu             sync.Mutex
	pollsUntilDone int
	opError        *caStatus
	final          caInvestigation
	createStatus   int
	createBody     string
	// createDelay and runDelay hold those replies back, to use up part
	// of the client's time budget before polling starts.
	createDelay time.Duration
	runDelay    time.Duration
	polls       int
	created     caInvestigation
	auth        []string
	paths       []string
}

func (f *fakeCloudAssist) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var delay time.Duration
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/investigations"):
		delay = f.createDelay
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":run"):
		delay = f.runDelay
	}
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1alpha/projects/p1/locations/global/investigations":
		if f.createStatus != 0 {
			http.Error(w, f.createBody, f.createStatus)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&f.created); err != nil {
			f.t.Errorf("decode create body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(caInvestigation{Name: testInvName, Revision: testRevName})
	case r.Method == http.MethodPost && r.URL.Path == "/v1alpha/"+testRevName+":run":
		_ = json.NewEncoder(w).Encode(caOperation{Name: testOpName})
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/"+testOpName:
		f.polls++
		op := caOperation{Name: testOpName, Done: f.polls >= f.pollsUntilDone}
		if op.Done {
			op.Error = f.opError
		}
		_ = json.NewEncoder(w).Encode(op)
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/"+testInvName:
		_ = json.NewEncoder(w).Encode(f.final)
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/projects/p1/locations/global/investigations":
		_, _ = w.Write([]byte(`{"investigations":[]}`))
	default:
		http.NotFound(w, r)
	}
}

func completedInvestigation() caInvestigation {
	return caInvestigation{
		Name:           testInvName,
		ExecutionState: caStateCompleted,
		Observations: map[string]caObservation{
			"user.input.log": {ID: "user.input.log", Text: "the issue", ObservationType: "OBSERVATION_TYPE_CLOUD_LOG", ObserverType: "OBSERVER_TYPE_USER"},
			"h.low":          {ID: "h.low", Title: "Quota exhausted", Text: "Maybe quota.", ObservationType: "OBSERVATION_TYPE_HYPOTHESIS", ObserverType: "OBSERVER_TYPE_AI", SystemRelevanceScore: 0.2},
			"h.high":         {ID: "h.high", Title: "Bad deploy", Text: "Revision 42 crashes on start.", Recommendation: "Roll back to revision 41.", ObservationType: "OBSERVATION_TYPE_HYPOTHESIS", ObserverType: "OBSERVER_TYPE_AI", SystemRelevanceScore: 0.9},
			"log.1":          {ID: "log.1", Title: "Errors in logs", Text: "500s since 10:02", ObservationType: "OBSERVATION_TYPE_CLOUD_LOG", ObserverType: "OBSERVER_TYPE_SIGNALS"},
			"kb":             {ID: "kb", Text: "doc link", ObservationType: "OBSERVATION_TYPE_KNOWLEDGE", ObserverType: "OBSERVER_TYPE_AI"},
		},
	}
}

func newTestCloudAssist(t *testing.T, f *fakeCloudAssist) (*CloudAssistClient, *httptest.Server) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := newCloudAssistClient(CloudAssistClientConfig{Project: "p1", Endpoint: srv.URL, PollInterval: time.Millisecond, PollTimeout: 5 * time.Second})
	c.tokenSource = fakeTokenSource{token: "fake-token"}
	c.now = func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }
	return c, srv
}

var alertPrompt = []Message{
	{Role: "system", Content: "you are an SRE"},
	{Role: "user", Content: "告警名稱：cloud_run_5xx\n主機：checkout\n相關 log：\n[10:02:00] 500"},
}

func TestCloudAssistClient_Chat(t *testing.T) {
	f := &fakeCloudAssist{pollsUntilDone: 3, final: completedInvestigation()}
	c, _ := newTestCloudAssist(t, f)

	reply, err := c.Chat(alertPrompt, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Request side: the flow gcloud uses, with the prompt as the issue.
	want := []string{
		"POST /v1alpha/projects/p1/locations/global/investigations",
		"POST /v1alpha/" + testRevName + ":run",
		"GET /v1alpha/" + testOpName,
		"GET /v1alpha/" + testOpName,
		"GET /v1alpha/" + testOpName,
		"GET /v1alpha/" + testInvName,
	}
	if strings.Join(f.paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(f.paths, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range f.auth {
		if a != "Bearer fake-token" {
			t.Errorf("Authorization = %q, want the ADC token on every call", a)
		}
	}
	if f.created.Title != "victoria-gateway: 告警名稱：cloud_run_5xx" {
		t.Errorf("title = %q", f.created.Title)
	}
	proj := f.created.Observations["user.project"]
	if proj.Text != "p1" || proj.ObservationType != "OBSERVATION_TYPE_STRUCTURED_INPUT" || proj.ObserverType != "OBSERVER_TYPE_USER" {
		t.Errorf("user.project observation = %+v", proj)
	}
	issue := f.created.Observations["user.input.log"]
	if issue.Text != alertPrompt[1].Content || issue.ObservationType != "OBSERVATION_TYPE_CLOUD_LOG" || issue.ObserverType != "OBSERVER_TYPE_USER" {
		t.Errorf("user.input.log observation = %+v", issue)
	}
	if len(issue.TimeIntervals) != 1 || issue.TimeIntervals[0].StartTime != "2026-09-27T09:00:00Z" {
		t.Errorf("timeIntervals = %+v, want one starting an hour before now", issue.TimeIntervals)
	}

	// Result side: hypotheses by relevance, user input and knowledge
	// observations left out.
	if !strings.Contains(reply, "investigation "+testInvName) {
		t.Errorf("reply doesn't name the investigation:\n%s", reply)
	}
	hi, lo := strings.Index(reply, "Hypothesis 1: Bad deploy"), strings.Index(reply, "Hypothesis 2: Quota exhausted")
	if hi < 0 || lo < 0 || hi > lo {
		t.Errorf("hypotheses missing or out of order:\n%s", reply)
	}
	for _, w := range []string{"Revision 42 crashes on start.", "Recommendation: Roll back to revision 41."} {
		if !strings.Contains(reply, w) {
			t.Errorf("reply missing %q:\n%s", w, reply)
		}
	}
	for _, unwanted := range []string{"the issue", "doc link"} {
		if strings.Contains(reply, unwanted) {
			t.Errorf("reply includes %q, which isn't a finding:\n%s", unwanted, reply)
		}
	}
}

func TestCloudAssistClient_SummaryObservationComesFirst(t *testing.T) {
	inv := completedInvestigation()
	inv.Observations["sum"] = caObservation{ID: "sum", Title: "Investigation summary", Text: "Bad deploy at 10:01.", ObservationType: "OBSERVATION_TYPE_INVESTIGATION_SUMMARY", ObserverType: "OBSERVER_TYPE_AI"}
	got, err := formatInvestigation(inv)
	if err != nil {
		t.Fatal(err)
	}
	if s, h := strings.Index(got, "Bad deploy at 10:01."), strings.Index(got, "Hypothesis 1"); s < 0 || s > h {
		t.Errorf("summary should precede the hypotheses:\n%s", got)
	}
}

func TestCloudAssistClient_NoHypothesesListsObservations(t *testing.T) {
	inv := completedInvestigation()
	delete(inv.Observations, "h.low")
	delete(inv.Observations, "h.high")
	got, err := formatInvestigation(inv)
	if err != nil || !strings.Contains(got, "No hypothesis was produced") || !strings.Contains(got, "- Errors in logs") {
		t.Errorf("got (%q, %v)", got, err)
	}
	delete(inv.Observations, "log.1")
	if _, err := formatInvestigation(inv); err == nil || !strings.Contains(err.Error(), "no hypotheses or observations") {
		t.Errorf("empty investigation: err = %v", err)
	}
}

func TestCloudAssistClient_PollTimeout(t *testing.T) {
	f := &fakeCloudAssist{pollsUntilDone: 1 << 30, final: completedInvestigation()}
	c, _ := newTestCloudAssist(t, f)
	c.now = time.Now
	c.pollTimeout = 30 * time.Millisecond

	start := time.Now()
	_, err := c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "did not complete within 30ms") || !strings.Contains(err.Error(), testInvName) {
		t.Fatalf("err = %v, want a timeout naming the investigation", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %s, want it to stop near the poll timeout", time.Since(start))
	}
	if f.polls < 2 {
		t.Errorf("polls = %d, want it to have polled more than once", f.polls)
	}
}

// create and run share the poll budget: when they are slow enough to use
// it all up, Chat stops at the deadline with the same "did not complete
// within" error instead of starting a fresh poll wait after them.
func TestCloudAssistClient_SlowCreateAndRunShareTheBudget(t *testing.T) {
	f := &fakeCloudAssist{pollsUntilDone: 1 << 30, createDelay: 60 * time.Millisecond, runDelay: 60 * time.Millisecond}
	c, _ := newTestCloudAssist(t, f)
	c.now = time.Now
	c.pollTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "did not complete within 100ms") || !strings.Contains(err.Error(), testInvName) {
		t.Fatalf("err = %v, want the timeout naming the investigation", err)
	}
	if f.polls != 0 {
		t.Errorf("polls = %d, want 0: create and run already used the whole budget", f.polls)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %s, want it to stop near the 100ms budget", el)
	}
}

// When the budget runs out in the middle of a poll request, the error is
// still the timeout, not a context error from the poll.
func TestCloudAssistClient_BudgetEndsDuringPoll(t *testing.T) {
	f := &fakeCloudAssist{pollsUntilDone: 1 << 30}
	c, srv := newTestCloudAssist(t, f)
	c.now = time.Now
	c.pollTimeout = 100 * time.Millisecond
	c.client.Timeout = 80 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var polls atomic.Int32
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1alpha/"+testOpName {
			if polls.Add(1) == 1 {
				// The first poll is slow but answers "not done yet"...
				time.Sleep(60 * time.Millisecond)
				inner.ServeHTTP(w, r)
				return
			}
			// ...and the second, sent before the deadline, never answers,
			// so its HTTP timeout fires after the deadline.
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		inner.ServeHTTP(w, r)
	})

	_, err := c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "did not complete within 100ms") {
		t.Fatalf("err = %v, want the friendly timeout", err)
	}
}

func TestCloudAssistClient_RunFailed(t *testing.T) {
	f := &fakeCloudAssist{pollsUntilDone: 1, opError: &caStatus{Code: 13, Message: "An internal error has occurred"}}
	c, _ := newTestCloudAssist(t, f)
	_, err := c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "An internal error has occurred (code 13)") || !strings.Contains(err.Error(), "Premium Support") {
		t.Errorf("err = %v, want the operation error plus the access hint", err)
	}

	inv := completedInvestigation()
	inv.ExecutionState = "INVESTIGATION_EXECUTION_STATE_FAILED"
	inv.Error = &caStatus{Code: 9, Message: "observer blocked"}
	f = &fakeCloudAssist{pollsUntilDone: 1, final: inv}
	c, _ = newTestCloudAssist(t, f)
	_, err = c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "INVESTIGATION_EXECUTION_STATE_FAILED: observer blocked") {
		t.Errorf("err = %v, want the failed execution state", err)
	}
}

// A done run whose investigation isn't COMPLETED still returns what it
// found, but says which state it was in; with nothing found, the error
// names the state.
func TestCloudAssistClient_DoneButNotCompleted(t *testing.T) {
	inv := completedInvestigation()
	inv.ExecutionState = "INVESTIGATION_EXECUTION_STATE_RUNNING"
	c, _ := newTestCloudAssist(t, &fakeCloudAssist{pollsUntilDone: 1, final: inv})
	reply, err := c.Chat(alertPrompt, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(reply, "Hypothesis 1: Bad deploy") || !strings.Contains(reply, "execution state is INVESTIGATION_EXECUTION_STATE_RUNNING") {
		t.Errorf("reply should keep the findings and flag the state:\n%s", reply)
	}

	empty := caInvestigation{Name: testInvName}
	c, _ = newTestCloudAssist(t, &fakeCloudAssist{pollsUntilDone: 1, final: empty})
	_, err = c.Chat(alertPrompt, nil)
	if err == nil || !strings.Contains(err.Error(), "no hypotheses or observations (execution state unspecified)") {
		t.Errorf("err = %v, want the empty-result error with the state", err)
	}

	// COMPLETED output has no such note.
	c, _ = newTestCloudAssist(t, &fakeCloudAssist{pollsUntilDone: 1, final: completedInvestigation()})
	if reply, err := c.Chat(alertPrompt, nil); err != nil || strings.Contains(reply, "execution state") {
		t.Errorf("completed: (%q, %v), want no state note", reply, err)
	}
}

// Error codes keep their *HTTPStatusError type (5xx/429 read as
// unavailable), and the setup failures come with what to do about them.
func TestCloudAssistClient_HTTPErrors(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantHint string
	}{
		{403, `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`, "enable the Gemini Cloud Assist API (geminicloudassist.googleapis.com) on project p1"},
		{403, `{"error":{"status":"PERMISSION_DENIED","message":"denied"}}`, "roles/geminicloudassist.investigationCreator"},
		{404, `{"error":{"status":"NOT_FOUND"}}`, `only serves location "global"`},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, ""},
		{503, `{"error":{"status":"UNAVAILABLE"}}`, ""},
	}
	for _, tc := range cases {
		f := &fakeCloudAssist{createStatus: tc.status, createBody: tc.body}
		c, _ := newTestCloudAssist(t, f)
		_, err := c.Chat(alertPrompt, nil)
		var se *HTTPStatusError
		if !errors.As(err, &se) || se.StatusCode != tc.status || se.Backend != "gcp-cloud-assist" {
			t.Errorf("status %d: err = %v, want *HTTPStatusError", tc.status, err)
			continue
		}
		if tc.wantHint != "" && !strings.Contains(err.Error(), tc.wantHint) {
			t.Errorf("status %d: err = %v, want hint %q", tc.status, err, tc.wantHint)
		}
		if len(f.paths) != 1 {
			t.Errorf("status %d: %d requests, want to stop after the failed create", tc.status, len(f.paths))
		}
	}
}

func TestCloudAssistClient_CredentialsAndInput(t *testing.T) {
	f := &fakeCloudAssist{}
	c, _ := newTestCloudAssist(t, f)
	c.loadErr = errors.New("could not find default credentials")
	if _, err := c.Chat(alertPrompt, nil); err == nil || !strings.Contains(err.Error(), "Application Default Credentials") {
		t.Errorf("loadErr: err = %v", err)
	}
	if c.Available() {
		t.Error("Available() = true without credentials")
	}
	c, _ = newTestCloudAssist(t, f)
	c.tokenSource = fakeTokenSource{err: errors.New("refresh failed")}
	if _, err := c.Chat(alertPrompt, nil); err == nil || !strings.Contains(err.Error(), "get access token") {
		t.Errorf("token error: err = %v", err)
	}
	if _, err := c.Chat([]Message{{Role: "system", Content: "only system"}}, nil); err == nil || !strings.Contains(err.Error(), "no user message") {
		t.Errorf("no user message: err = %v", err)
	}
	if len(f.paths) != 0 {
		t.Errorf("requests = %v, want none", f.paths)
	}
}

func TestCloudAssistClient_Available(t *testing.T) {
	f := &fakeCloudAssist{}
	c, _ := newTestCloudAssist(t, f)
	if !c.Available() {
		t.Error("Available() = false against a working list endpoint")
	}
	if c.Backend() != "gcp-cloud-assist" || c.ModelName() != "gcp-cloud-assist" {
		t.Errorf("Backend/ModelName = %q/%q", c.Backend(), c.ModelName())
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "告警ab"
	for n, want := range map[int]string{0: "", 2: "", 3: "告", 5: "告", 6: "告警", 7: "告警a", 100: s} {
		if got := truncateUTF8(s, n); got != want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", s, n, got, want)
		}
	}
	long := strings.Repeat("字", cloudAssistMaxIssueBytes)
	f := &fakeCloudAssist{pollsUntilDone: 1, final: completedInvestigation()}
	c, _ := newTestCloudAssist(t, f)
	if _, err := c.Chat([]Message{{Role: "user", Content: long}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.created.Observations["user.input.log"].Text; len(got) > cloudAssistMaxIssueBytes || !strings.HasPrefix(long, got) {
		t.Errorf("issue text is %d bytes, want at most %d and a prefix of the prompt", len(got), cloudAssistMaxIssueBytes)
	}
}
