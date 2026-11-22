package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The gateway speaks the OpenAI chat-completions dialect, which every client
// library already understands, and translates to Gemini's generateContent.

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	User        string        `json:"user,omitempty"`
}

type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Text returns the message content, accepting both the string form and the
// array-of-parts form ([{"type":"text","text":"…"}]).
func (m ChatMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func textContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

type ChatResponse struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []Choice   `json:"choices"`
	Usage   *ChatUsage `json:"usage,omitempty"`
}

type Choice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageFrom(u *Usage) ChatUsage {
	if u == nil {
		return ChatUsage{}
	}
	out := u.CandidatesTokenCount + u.ThoughtsTokenCount // thinking tokens are billed as output
	return ChatUsage{PromptTokens: u.PromptTokenCount, CompletionTokens: out, TotalTokens: u.PromptTokenCount + out}
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

var errNoMessages = errors.New("messages must contain at least one message")

// toGemini maps roles (system → systemInstruction, assistant → model) and
// sampling parameters. Gemini 3 models think by default; LOW keeps latency and
// cost predictable for a gateway and can be overridden per deployment.
func toGemini(req *ChatRequest, thinking string) (*GeminiRequest, error) {
	if len(req.Messages) == 0 {
		return nil, errNoMessages
	}
	g := &GeminiRequest{}
	var sys []string
	for _, m := range req.Messages {
		text := m.Text()
		switch m.Role {
		case "system", "developer":
			sys = append(sys, text)
		case "assistant":
			g.Contents = append(g.Contents, GContent{Role: "model", Parts: []GPart{{Text: text}}})
		default:
			g.Contents = append(g.Contents, GContent{Role: "user", Parts: []GPart{{Text: text}}})
		}
	}
	if len(g.Contents) == 0 {
		return nil, errNoMessages
	}
	if len(sys) > 0 {
		g.SystemInstruction = &GContent{Parts: []GPart{{Text: strings.Join(sys, "\n\n")}}}
	}
	gc := &GenConfig{MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP}
	if gc.MaxOutputTokens == 0 {
		gc.MaxOutputTokens = 1024
	}
	if thinking != "" && strings.HasPrefix(req.Model, "gemini-3") {
		gc.ThinkingConfig = &ThinkingConfig{ThinkingLevel: thinking}
	}
	g.GenerationConfig = gc
	return g, nil
}

func openAIFinish(g string) string {
	switch g {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

func lastUserText(req *ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return strings.TrimSpace(req.Messages[i].Text())
		}
	}
	return ""
}

// cacheKey is a hash of everything that changes the answer: messages, model
// and sampling parameters, scoped to the tenant namespace by the caller.
func cacheKey(ns string, req *ChatRequest) string {
	h := sha256.New()
	h.Write([]byte(ns))
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte{1})
		h.Write([]byte(strings.TrimSpace(m.Text())))
	}
	params, _ := json.Marshal(struct {
		M int      `json:"m"`
		T *float64 `json:"t"`
		P *float64 `json:"p"`
	}{req.MaxTokens, req.Temperature, req.TopP})
	h.Write(params)
	return hex.EncodeToString(h.Sum(nil))
}

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func nowUnix() int64 { return time.Now().Unix() }
