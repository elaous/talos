package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/elaous/talos/config"
	"github.com/elaous/talos/llm"
	"github.com/elaous/talos/rag"
	"github.com/elaous/talos/web"
)

type script struct {
	replies []string
	seen    [][]llm.Message
}

func (s *script) Chat(_ context.Context, model string, m []llm.Message) (string, error) {
	s.seen = append(s.seen, m)
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func TestExtractJSON(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"final\": \"a {b}\"}\n```": "a {b}",
		"Sure! {\"final\": \"x\"} done":        "x",
		"{oops} {\"final\": \"y\"}":            "y",
	} {
		m, ok := ExtractJSON(in)
		if !ok || m["final"] != want {
			t.Errorf("%q -> %v %v, want %q", in, m, ok, want)
		}
	}
	if _, ok := ExtractJSON("no json here"); ok {
		t.Error("found json in prose")
	}
}

func TestSupervisorDelegatesToSpecialists(t *testing.T) {
	s := &script{replies: []string{
		`{"tool":"ask_web_researcher","args":{"task":"latest go release"}}`, // supervisor
		`{"tool":"web_search","args":{"query":"go release"}}`,               // web_researcher -> fails offline, sees the error
		`{"final":"Go 1.26 [https://go.dev]"}`,                              // web_researcher
		`{"tool":"ask_writer","args":{"task":"notes: Go 1.26"}}`,            // supervisor
		`{"final":"Go 1.26 is current [https://go.dev]."}`,                  // writer
		`{"final":"Go 1.26 is current [https://go.dev]."}`,                  // supervisor
	}}
	var cfg config.Config
	cfg.MaxSteps = 4
	team := NewTeam(cfg, s, &rag.Library{}, web.New("http://127.0.0.1:1", false))
	var trace []Event
	ans, err := team.Ask(context.Background(), "what is the latest Go?", nil, func(e Event) { trace = append(trace, e) })
	if err != nil || !strings.Contains(ans, "Go 1.26") {
		t.Fatalf("ans %q err %v", ans, err)
	}
	var agents []string
	for _, e := range trace {
		agents = append(agents, e.Agent+":"+e.Kind)
	}
	got := strings.Join(agents, " ")
	for _, want := range []string{"supervisor:call", "web_researcher:call", "web_researcher:result", "writer:final", "supervisor:final"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace %q lacks %s", got, want)
		}
	}
}

func TestStepLimitForcesAnAnswer(t *testing.T) {
	loop := `{"tool":"nope","args":{}}`
	s := &script{replies: []string{loop, loop, `{"final":"gave up"}`}}
	a := &Agent{Name: "x", Chat: s, MaxSteps: 2, Tools: []Tool{{Name: "real"}}}
	ans, err := a.Run(context.Background(), "q", nil)
	if err != nil || ans != "gave up" {
		t.Fatalf("%q %v", ans, err)
	}
	if last := s.seen[1][len(s.seen[1])-1].Content; !strings.Contains(last, "Unknown tool") {
		t.Errorf("model was not told the tool is unknown: %q", last)
	}
}
