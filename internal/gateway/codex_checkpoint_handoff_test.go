package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/codexsummary"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// Same wire shape as Codex's native compaction, with synthetic ciphertext.
const handoffSeal = "gAAAAABcheckpoint_from_openai_only=="
const handoffInput = `{"model":"destination/m1","stream":true,"store":false,"previous_response_id":"resp_old",
 "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
 "input":[{"type":"compaction","id":"cmp_old","encrypted_content":"` + handoffSeal + `","extra":"CURRENT_CONTEXT_MUST_NOT_REACH_SOURCE"},
 {"type":"function_call","call_id":"call_one","name":"shell","arguments":"{}"},
 {"type":"function_call_output","call_id":"call_one","output":"ok"},
 {"role":"user","content":"CURRENT_CONTEXT_MUST_NOT_REACH_SOURCE"}]}`

func handoffSummary(text string) string {
	item, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []any{
		map[string]string{"type": "output_text", "text": text}}})
	return sse(`data: {"type":"response.output_item.done","item":`+string(item)+`}`,
		`data: {"type":"response.completed","response":{"id":"resp_summary","status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":7}}}`)
}

type handoffCapture struct {
	sync.Mutex
	bodies   []string
	headers  []http.Header
	status   int
	reply    string
	location string
}

func (c *handoffCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.Lock()
	c.bodies = append(c.bodies, string(b))
	c.headers = append(c.headers, r.Header.Clone())
	status, reply, location := c.status, c.reply, c.location
	c.Unlock()
	if location != "" {
		w.Header().Set("Location", location)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if status != 0 {
		w.WriteHeader(status)
	}
	io.WriteString(w, reply)
}

func (c *handoffCapture) count() int { c.Lock(); defer c.Unlock(); return len(c.bodies) }

func handoffSetup(t *testing.T, source *handoffCapture, on bool) (*Server, string, *handoffCapture) {
	t.Helper()
	fresh(t)
	origin := httptest.NewServer(source)
	t.Cleanup(origin.Close)
	was := provider.CodexBase
	provider.CodexBase = origin.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	target := &handoffCapture{reply: handoffSummary("destination answered")}
	upstream := httptest.NewServer(target)
	t.Cleanup(upstream.Close)
	if err := provider.Save(provider.Provider{ID: "destination", Name: "Destination", Responses: upstream.URL + "/v1", Key: "destination-key", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if on {
		if err := settings.Save(settings.Settings{CodexCheckpointModel: "gpt-6-luna"}); err != nil {
			t.Fatal(err)
		}
	}
	s := New()
	front := httptest.NewServer(s.Front())
	t.Cleanup(front.Close)
	return s, front.URL, target
}

func handoffPost(t *testing.T, base, body, token, account string) (int, string) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, base+CodexPath+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if account != "" {
		r.Header.Set("ChatGPT-Account-ID", account)
	}
	r.Header.Set("Cookie", "cookie-must-stay-local")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestCodexCheckpointHandoffRecoversBeforeRouting(t *testing.T) {
	source := &handoffCapture{reply: handoffSummary("Sentinel ORCHID-7924; parser.ts changed; tests not run.")}
	_, base, target := handoffSetup(t, source, true)
	for _, caller := range []struct{ token, account string }{
		{"caller-one", "account-one"}, {"caller-one", "account-one"},
		{"caller-two", "account-one"}, {"caller-two", "account-two"},
	} {
		if code, body := handoffPost(t, base, handoffInput, caller.token, caller.account); code != 200 {
			t.Fatalf("handoff: %d %s", code, body)
		}
	}
	if source.count() != 3 || target.count() != 4 {
		t.Fatalf("source %d target %d", source.count(), target.count())
	}
	source.Lock()
	for i, body := range source.bodies {
		var q map[string]any
		json.Unmarshal([]byte(body), &q)
		if q["model"] != "gpt-6-luna" || q["store"] != false || q["stream"] != true ||
			!strings.Contains(body, handoffSeal) || strings.Contains(body, "CURRENT_CONTEXT") || strings.Contains(body, "call_one") || q["tools"] != nil {
			t.Errorf("source got %s", body)
		}
		h := source.headers[i]
		wantToken := []string{"caller-one", "caller-two", "caller-two"}[i]
		wantAccount := []string{"account-one", "account-one", "account-two"}[i]
		if h.Get("Authorization") != "Bearer "+wantToken || h.Get("ChatGPT-Account-ID") != wantAccount {
			t.Errorf("wrong recovery caller: %v", h)
		}
		if h.Get("Cookie") != "" || strings.Contains(h.Get("Authorization"), "destination") || h.Get("ChatGPT-Account-ID") == "" {
			t.Errorf("source credential boundary: %v", h)
		}
	}
	source.Unlock()
	target.Lock()
	for i, body := range target.bodies {
		for _, want := range []string{"ORCHID-7924", "parser.ts", "tests not run", "call_one", "CURRENT_CONTEXT"} {
			if !strings.Contains(body, want) {
				t.Errorf("destination lost %q: %s", want, body)
			}
		}
		if strings.Contains(body, handoffSeal) || strings.Contains(body, "resp_old") {
			t.Errorf("opaque input leaked: %s", body)
		}
		h := target.headers[i]
		if h.Get("Authorization") != "Bearer destination-key" || h.Get("ChatGPT-Account-ID") != "" || h.Get("Cookie") != "" {
			t.Errorf("destination credential boundary: %v", h)
		}
	}
	target.Unlock()
	var recoveries int
	for _, row := range usage.Load(time.Time{}) {
		if row.Agent == "magpie" && row.Kind == "compaction" && row.Model == "gpt-6-luna" {
			recoveries++
			if row.Input != 19 || row.Output != 7 {
				t.Errorf("recovery usage: %+v", row)
			}
		}
	}
	if recoveries != 3 {
		t.Errorf("recovery usage rows: %d", recoveries)
	}
}

func TestCodexCheckpointHandoffOffByDefault(t *testing.T) {
	source := &handoffCapture{reply: handoffSummary("must not be asked")}
	_, base, target := handoffSetup(t, source, false)
	code, body := handoffPost(t, base, handoffInput, "caller", "account")
	if code != 200 || source.count() != 0 || target.count() != 1 {
		t.Fatalf("default: %d %s source=%d target=%d", code, body, source.count(), target.count())
	}
}

func TestCodexCheckpointHandoffFailuresRetainHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reply  string
	}{
		{"unauthorized", 401, "PRIVATE_ERROR_MUST_NOT_ESCAPE"},
		{"rate limited", 429, "PRIVATE_ERROR_MUST_NOT_ESCAPE"},
		{"unavailable", 503, "PRIVATE_ERROR_MUST_NOT_ESCAPE"},
		{"failed stream", 200, sse(`data: {"type":"response.failed","response":{"error":{"message":"PRIVATE_ERROR_MUST_NOT_ESCAPE"}}}`)},
		{"truncated stream", 200, sse(`data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"partial"}]}}`)},
		{"empty summary", 200, handoffSummary("")},
		{"incomplete", 200, `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`},
		{"tool call", 200, `{"status":"completed","output":[{"type":"function_call","name":"shell"},{"type":"message","content":[{"type":"output_text","text":"not a valid summary"}]}]}`},
		{"oversize reply", 200, strings.Repeat("x", handoffReplyMost+1)},
		{"oversize summary", 200, handoffSummary(strings.Repeat("x", handoffTextMost+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &handoffCapture{status: tc.status, reply: tc.reply}
			s, base, target := handoffSetup(t, source, true)
			for range 2 {
				code, body := handoffPost(t, base, handoffInput, "caller", "account")
				if code != 502 || !strings.Contains(body, "original history retained") || strings.Contains(body, "PRIVATE_ERROR") || strings.Contains(body, handoffSeal) {
					t.Fatalf("failure: %d %s", code, body)
				}
			}
			if target.count() != 0 || source.count() != 2 || len(s.checkpointHandoffs.summaries) != 0 {
				t.Fatalf("failure forwarded or cached: source=%d target=%d", source.count(), target.count())
			}
		})
	}
}

func TestCodexCheckpointHandoffRedirectAndAuth(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		other := &handoffCapture{reply: handoffSummary("must not be asked")}
		redirect := httptest.NewServer(other)
		defer redirect.Close()
		source := &handoffCapture{status: 307, location: redirect.URL}
		_, base, target := handoffSetup(t, source, true)
		code, _ := handoffPost(t, base, handoffInput, "caller", "account")
		if code != 502 || other.count() != 0 || target.count() != 0 {
			t.Fatalf("redirect: %d", code)
		}
	})
	for _, tc := range []struct{ name, token, account string }{
		{"no auth", "", "account"}, {"API key", "sk-private", "account"}, {"no account", "caller", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &handoffCapture{}
			_, base, target := handoffSetup(t, source, true)
			code, _ := handoffPost(t, base, handoffInput, tc.token, tc.account)
			if code < 400 || source.count() != 0 || target.count() != 0 {
				t.Fatalf("auth: %d source=%d target=%d", code, source.count(), target.count())
			}
		})
	}
}

func TestCodexCheckpointHandoffUnknownFormatAndSettings(t *testing.T) {
	for _, broken := range []bool{false, true} {
		source := &handoffCapture{}
		_, base, target := handoffSetup(t, source, true)
		body := strings.ReplaceAll(handoffInput, handoffSeal, "another-gateways-private-format")
		if broken {
			if err := os.WriteFile(settings.Path(), []byte(`{"codexCheckpointModel":`), 0600); err != nil {
				t.Fatal(err)
			}
			body = handoffInput
		}
		code, _ := handoffPost(t, base, body, "caller", "account")
		if code != 502 || source.count() != 0 || target.count() != 0 {
			t.Fatalf("unknown input: %d", code)
		}
	}
}

func TestCodexCheckpointHandoffPreservesOtherRoutes(t *testing.T) {
	t.Run("magpie summary", func(t *testing.T) {
		source := &handoffCapture{}
		_, base, target := handoffSetup(t, source, true)
		body := strings.ReplaceAll(handoffInput, handoffSeal, codexsummary.Encrypted("already portable"))
		code, reply := handoffPost(t, base, body, "caller", "account")
		if code != 200 || source.count() != 0 || target.count() != 1 {
			t.Fatalf("portable: %d %s", code, reply)
		}
	})
	t.Run("unknown destination", func(t *testing.T) {
		source := &handoffCapture{}
		_, base, target := handoffSetup(t, source, true)
		code, _ := handoffPost(t, base, strings.ReplaceAll(handoffInput, "destination/m1", "missing/m1"), "caller", "account")
		if code < 400 || source.count() != 0 || target.count() != 0 {
			t.Fatalf("unknown: %d", code)
		}
	})
	t.Run("native destination", func(t *testing.T) {
		source := &handoffCapture{}
		_, base, target := handoffSetup(t, source, true)
		nativeOn(t, "destination")
		code, reply := handoffPost(t, base, handoffInput, "caller", "account")
		if code != 200 || source.count() != 0 || target.count() != 1 {
			t.Fatalf("native: %d %s", code, reply)
		}
	})
	t.Run("known writer", func(t *testing.T) {
		source := &handoffCapture{}
		_, base, target := handoffSetup(t, source, true)
		nativeOn(t, "destination")
		target.Lock()
		target.reply = nativeSealedReply(handoffSeal)
		target.Unlock()
		code, body := handoffPost(t, base, strings.Replace(compactTurn, "%s", "destination/m1", 1), "caller", "account")
		if code != 200 {
			t.Fatalf("record checkpoint writer: %d %s", code, body)
		}
		p, _, _ := provider.Resolve("destination/m1")
		p.ID, p.NativeCompaction = "other", false
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		code, body = handoffPost(t, base, strings.ReplaceAll(handoffInput, "destination/m1", "other/m1"), "caller", "account")
		if code != 400 || !strings.Contains(body, "compacted natively") || source.count() != 0 || target.count() != 1 {
			t.Fatalf("writer guard: %d %s source=%d target=%d", code, body, source.count(), target.count())
		}
	})
}

func TestCodexCheckpointHandoffCacheBoundAndModel(t *testing.T) {
	source := &handoffCapture{reply: handoffSummary("ORCHID-7924")}
	_, base, _ := handoffSetup(t, source, true)
	for i := range handoffCacheMost + 1 {
		body := strings.ReplaceAll(handoffInput, handoffSeal, fmt.Sprintf("gAAAAABcheckpoint_%d==", i))
		if code, reply := handoffPost(t, base, body, "caller", "account"); code != 200 {
			t.Fatalf("fill: %d %s", code, reply)
		}
	}
	body := strings.ReplaceAll(handoffInput, handoffSeal, "gAAAAABcheckpoint_0==")
	if code, _ := handoffPost(t, base, body, "caller", "account"); code != 200 || source.count() != handoffCacheMost+2 {
		t.Fatalf("oldest entry not evicted: status=%d source=%d", code, source.count())
	}
	if err := settings.Save(settings.Settings{CodexCheckpointModel: "gpt-another"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := handoffPost(t, base, body, "caller", "account"); code != 200 || source.count() != handoffCacheMost+3 {
		t.Fatalf("model not part of cache identity: status=%d source=%d", code, source.count())
	}
}

func TestCodexCheckpointHandoffDoesNotBorrowCredentials(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		source := &handoffCapture{}
		s, _, target := handoffSetup(t, source, true)
		r := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(handoffInput))
		r.Header.Set("Authorization", "Bearer caller")
		r.Header.Set("ChatGPT-Account-ID", "account")
		if keyed {
			r = r.WithContext(access.WithIdentity(r.Context(), access.Identity{KeyID: "limited-key"}))
		} else {
			r.Header.Set(AccountHeader, "different-saved-account")
		}
		body, err := s.recoverCodexCheckpoints(r, []byte(handoffInput))
		if err == nil || string(body) != handoffInput || source.count() != 0 || target.count() != 0 {
			t.Fatal("borrowed an account or mutated history")
		}
	}
	source := &handoffCapture{}
	s, _, _ := handoffSetup(t, source, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer caller")
	r.Header.Set("ChatGPT-Account-ID", "account")
	body, err := s.recoverCodexCheckpoints(r, []byte(handoffInput))
	if err == nil || string(body) != handoffInput || source.count() != 0 {
		t.Fatal("canceled recovery sent data or mutated history")
	}
}
