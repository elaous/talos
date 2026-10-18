// Package agent runs tool-using LLM agents. The tool protocol is plain JSON in the model's reply, so it works
// with every chat model, with or without native function calling:
//
//	{"tool": "<name>", "args": {...}}   call a tool
//	{"final": "<answer>"}               finish
//
// A supervisor treats its specialist agents as tools, which is the whole multi-agent mechanism.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/elaous/talos/llm"
)

// Tool is something an agent can call. Args describes the parameters for the prompt.
type Tool struct {
	Name, Description string
	Args              map[string]string
	Run               func(ctx context.Context, args map[string]any) (string, error)
}

// Event is one step of an agent's work, for the UI's trace.
type Event struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"` // call, result, final, note
	Text  string `json:"text"`
}

type Agent struct {
	Name, Role string
	Model      string
	Tools      []Tool
	Chat       llm.Chat
	MaxSteps   int
	Trace      func(Event)
}

const protocol = `You can use tools. Reply with ONLY one JSON object, no prose, no code fences:
  {"tool": "<tool name>", "args": {<arguments>}}      to call a tool
  {"final": "<your answer>"}                           when you are done
Tools:
%s
Rules: call tools only when needed; never invent tool results; cite sources (file p.N or URL) in the final answer.`

func (a *Agent) system() string {
	if len(a.Tools) == 0 {
		return a.Role + "\n\nReply with ONLY {\"final\": \"<answer>\"}."
	}
	var sb strings.Builder
	for _, t := range a.Tools {
		keys := make([]string, 0, len(t.Args))
		for k := range t.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args := make([]string, len(keys))
		for i, k := range keys {
			args[i] = k + ": " + t.Args[k]
		}
		fmt.Fprintf(&sb, "- %s(%s): %s\n", t.Name, strings.Join(args, "; "), t.Description)
	}
	return a.Role + "\n\n" + fmt.Sprintf(protocol, sb.String())
}

func (a *Agent) emit(kind, text string) {
	if a.Trace != nil {
		a.Trace(Event{Agent: a.Name, Kind: kind, Text: text})
	}
}

// Run works on task, with optional earlier conversation, and returns the final answer.
func (a *Agent) Run(ctx context.Context, task string, history []llm.Message) (string, error) {
	msgs := append([]llm.Message{{Role: "system", Content: a.system()}}, history...)
	msgs = append(msgs, llm.Message{Role: "user", Content: task})
	tools := map[string]Tool{}
	for _, t := range a.Tools {
		tools[t.Name] = t
	}
	steps := a.MaxSteps
	if steps <= 0 {
		steps = 6
	}
	for i := 0; i <= steps; i++ {
		if i == steps {
			msgs = append(msgs, llm.Message{Role: "user", Content: `Step limit reached. Reply now with {"final": "..."} using what you have.`})
		}
		raw, err := a.Chat.Chat(ctx, a.Model, msgs)
		if err != nil {
			return "", err
		}
		call, ok := ExtractJSON(raw)
		if !ok { // the model answered in prose: take it as the answer
			a.emit("final", strings.TrimSpace(raw))
			return strings.TrimSpace(raw), nil
		}
		if f, ok := call["final"]; ok {
			ans := asText(f)
			a.emit("final", ans)
			return ans, nil
		}
		if i == steps {
			return "I could not finish within the step limit.", nil
		}
		name, _ := call["tool"].(string)
		args, _ := call["args"].(map[string]any)
		argJSON, _ := json.Marshal(args)
		a.emit("call", name+" "+string(argJSON))
		var obs string
		if t, ok := tools[name]; !ok {
			obs = fmt.Sprintf("Unknown tool %q. Available: %s", name, a.toolNames())
		} else if out, err := t.Run(ctx, args); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			obs = "Tool error: " + err.Error() // let the model see it and recover
		} else {
			obs = out
		}
		a.emit("result", clip(obs, 600))
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: raw},
			llm.Message{Role: "user", Content: "Tool result:\n" + clip(obs, 8000) + "\n\nContinue."})
	}
	return "", fmt.Errorf("agent %s: unreachable", a.Name)
}

func (a *Agent) toolNames() string {
	var n []string
	for _, t := range a.Tools {
		n = append(n, t.Name)
	}
	return strings.Join(n, ", ")
}

func asText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// ExtractJSON finds the first JSON object in a model reply, tolerating code fences and chatter around it.
func ExtractJSON(s string) (map[string]any, bool) {
	for start := strings.IndexByte(s, '{'); start >= 0; {
		dec := json.NewDecoder(strings.NewReader(s[start:]))
		var m map[string]any
		if dec.Decode(&m) == nil && m != nil {
			return m, true
		}
		next := strings.IndexByte(s[start+1:], '{')
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return nil, false
}
