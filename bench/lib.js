// Shared helpers for llmgate k6 scripts.
// Env: BASE_URL (default http://localhost:8080), API_KEY (default demo-key),
//      MODEL (route alias, default per script).
export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
export const API_KEY = __ENV.API_KEY || 'demo-key';

export function chatBody(model, stream, maxTokens) {
  return JSON.stringify({
    model: model,
    stream: stream,
    max_tokens: maxTokens || 64,
    messages: [
      { role: 'system', content: 'You are a benchmark echo target.' },
      { role: 'user', content: 'Explain in one short paragraph what an LLM gateway does.' },
    ],
  });
}

export const jsonHeaders = {
  'Content-Type': 'application/json',
  Authorization: `Bearer ${API_KEY}`,
};

export function chatURL() {
  return `${BASE_URL}/v1/chat/completions`;
}
