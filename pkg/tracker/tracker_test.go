package tracker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/gitea"
	"github.com/gordonwei/victoria-gateway/pkg/github"
	"github.com/gordonwei/victoria-gateway/pkg/tracker"
)

// pkg/tracker is only the interface, so what is worth testing here is the
// contract: both implementations must satisfy it (compile-time) and must
// behave the same way through it against the same scenario.
var (
	_ tracker.Tracker = (*gitea.Client)(nil)
	_ tracker.Tracker = (*github.Client)(nil)
)

// fakeForge is an in-memory Gitea/GitHub issue API: both APIs share the
// same shapes for the calls a Tracker makes and differ only in a path
// prefix (Gitea's /api/v1), which the handler strips.
type fakeForge struct {
	mu       sync.Mutex
	state    map[int64]string
	comments map[int64][]string
	next     int64
	calls    []string // "METHOD path", in order
}

var issuePath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues(?:/(\d+)(/comments)?)?$`)

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	f.calls = append(f.calls, r.Method+" "+path)
	m := issuePath.FindStringSubmatch(path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)

	if m[1] == "" { // /issues
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		f.next++
		f.state[f.next] = "open"
		_ = json.NewEncoder(w).Encode(map[string]any{"number": f.next, "state": "open", "title": body["title"]})
		return
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if _, ok := f.state[n]; !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case m[2] == "/comments" && r.Method == http.MethodPost:
		f.comments[n] = append(f.comments[n], body["body"])
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	case m[2] == "/comments" && r.Method == http.MethodGet:
		out := []map[string]string{}
		// Oldest first, which is Gitea's order. GitHub is asked for newest
		// first with per_page=1, so honor that when it is asked.
		list := f.comments[n]
		if r.URL.Query().Get("direction") == "desc" && len(list) > 0 {
			list = []string{list[len(list)-1]}
		}
		for _, c := range list {
			out = append(out, map[string]string{"body": c})
		}
		_ = json.NewEncoder(w).Encode(out)
	case m[2] == "" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": f.state[n]})
	case m[2] == "" && r.Method == http.MethodPatch:
		f.state[n] = body["state"]
		_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": f.state[n]})
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

type impl struct {
	name string
	make func(url string) tracker.Tracker
}

var impls = []impl{
	{"gitea", func(url string) tracker.Tracker {
		return gitea.NewClient(gitea.ClientConfig{Endpoint: url, Token: "t", Owner: "o", Repo: "r"})
	}},
	{"github", func(url string) tracker.Tracker {
		return github.NewClient(github.ClientConfig{Endpoint: url, Token: "t", Owner: "o", Repo: "r"})
	}},
}

func TestTrackerContract_IssueLifecycle(t *testing.T) {
	for _, im := range impls {
		t.Run(im.name, func(t *testing.T) {
			f := &fakeForge{state: map[int64]string{}, comments: map[int64][]string{}}
			srv := httptest.NewServer(f)
			defer srv.Close()
			tr := im.make(srv.URL)
			ctx := context.Background()

			n, err := tr.CreateIssue(ctx, "[HighCPU] web01", "body")
			if err != nil || n != 1 {
				t.Fatalf("CreateIssue = (%d, %v), want (1, nil)", n, err)
			}
			if st, err := tr.IssueState(ctx, n); err != nil || st != "open" {
				t.Fatalf("IssueState = (%q, %v), want open", st, err)
			}
			if c, err := tr.LastComment(ctx, n); err != nil || c != "" {
				t.Fatalf("LastComment on a fresh issue = (%q, %v), want empty", c, err)
			}

			// Two comments; the last one is the resolution.
			f.comments[n] = []string{"looking into it"}
			if err := tr.CloseWithComment(ctx, n, "root cause: cron job, disabled it"); err != nil {
				t.Fatalf("CloseWithComment: %v", err)
			}
			if st, _ := tr.IssueState(ctx, n); st != "closed" {
				t.Errorf("state after CloseWithComment = %q, want closed", st)
			}
			if c, _ := tr.LastComment(ctx, n); c != "root cause: cron job, disabled it" {
				t.Errorf("LastComment = %q, want the closing comment", c)
			}

			// The comment must be posted before the close, so a failed
			// close never loses the resolution.
			var postAt, patchAt = -1, -1
			for i, c := range f.calls {
				if strings.HasPrefix(c, "POST") && strings.HasSuffix(c, "/comments") {
					postAt = i
				}
				if strings.HasPrefix(c, "PATCH") {
					patchAt = i
				}
			}
			if postAt < 0 || patchAt < 0 || postAt > patchAt {
				t.Errorf("call order = %v, want the comment POST before the PATCH", f.calls)
			}
		})
	}
}

func TestTrackerContract_ErrorPaths(t *testing.T) {
	status := func(code int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
	}
	cases := []struct {
		name string
		srv  *httptest.Server
	}{
		{"server error", status(http.StatusInternalServerError, "boom")},
		{"not found", status(http.StatusNotFound, `{"message":"Not Found"}`)},
		{"unauthorized", status(http.StatusUnauthorized, "bad token")},
		{"malformed JSON", status(http.StatusOK, "not json")},
	}
	for _, im := range impls {
		for _, tc := range cases {
			t.Run(im.name+"/"+tc.name, func(t *testing.T) {
				defer tc.srv.Close()
				tr := im.make(tc.srv.URL)
				ctx := context.Background()
				if _, err := tr.CreateIssue(ctx, "t", "b"); err == nil {
					t.Error("CreateIssue: want an error")
				}
				if _, err := tr.IssueState(ctx, 1); err == nil {
					t.Error("IssueState: want an error")
				}
				if _, err := tr.LastComment(ctx, 1); err == nil {
					t.Error("LastComment: want an error")
				}
				// A 2xx with a non-JSON body is fine for CloseWithComment
				// (it ignores the response body), so only the failing
				// statuses must error.
				if tc.name != "malformed JSON" {
					if err := tr.CloseWithComment(ctx, 1, "c"); err == nil {
						t.Error("CloseWithComment: want an error")
					}
				}
			})
		}
	}
}

func TestTrackerContract_CloseFailureKeepsComment(t *testing.T) {
	for _, im := range impls {
		t.Run(im.name, func(t *testing.T) {
			f := &fakeForge{state: map[int64]string{1: "open"}, comments: map[int64][]string{}}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					http.Error(w, "nope", http.StatusForbidden)
					return
				}
				f.ServeHTTP(w, r)
			}))
			defer srv.Close()

			err := im.make(srv.URL).CloseWithComment(context.Background(), 1, "the resolution")
			if err == nil {
				t.Fatal("want an error when the close fails")
			}
			if got := f.comments[1]; len(got) != 1 || got[0] != "the resolution" {
				t.Errorf("comments = %v, want the resolution kept even though the close failed", got)
			}
		})
	}
}
