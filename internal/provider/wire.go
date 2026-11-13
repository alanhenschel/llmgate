package provider

import (
	"encoding/json"
	"time"
)

// OpenAI wire types used for everything we synthesize or parse.

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type wireChoice struct {
	Index        int          `json:"index"`
	Message      *wireMessage `json:"message,omitempty"`
	Delta        *wireDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type wireCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage,omitempty"`
}

func completionBody(id, model, content, finish string, u Usage) []byte {
	b, _ := json.Marshal(wireCompletion{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []wireChoice{{Message: &wireMessage{Role: "assistant", Content: content}, FinishReason: &finish}},
		Usage:   &wireUsage{u.PromptTokens, u.CompletionTokens, u.PromptTokens + u.CompletionTokens},
	})
	return b
}

type chunkBuilder struct {
	id, model string
	created   int64
}

func newChunkBuilder(id, model string) chunkBuilder {
	return chunkBuilder{id: id, model: model, created: time.Now().Unix()}
}

func (b chunkBuilder) marshal(choices []wireChoice, u *wireUsage) []byte {
	out, _ := json.Marshal(wireCompletion{
		ID: b.id, Object: "chat.completion.chunk", Created: b.created, Model: b.model,
		Choices: choices, Usage: u,
	})
	return out
}

func (b chunkBuilder) role() Chunk {
	return Chunk{Data: b.marshal([]wireChoice{{Delta: &wireDelta{Role: "assistant"}}}, nil)}
}

func (b chunkBuilder) content(s string) Chunk {
	return Chunk{Data: b.marshal([]wireChoice{{Delta: &wireDelta{Content: s}}}, nil)}
}

func (b chunkBuilder) finish(reason string) Chunk {
	return Chunk{Data: b.marshal([]wireChoice{{Delta: &wireDelta{}, FinishReason: &reason}}, nil)}
}

func (b chunkBuilder) usage(u Usage) Chunk {
	return Chunk{
		Data:      b.marshal([]wireChoice{}, &wireUsage{u.PromptTokens, u.CompletionTokens, u.PromptTokens + u.CompletionTokens}),
		UsageOnly: true,
	}
}
