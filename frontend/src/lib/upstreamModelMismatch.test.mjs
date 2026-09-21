import assert from "node:assert/strict";
import test from "node:test";

import { isUpstreamModelMismatch } from "./upstreamModelMismatch.ts";

test("identical or missing models are not a mismatch", () => {
  assert.equal(isUpstreamModelMismatch("gpt-5.4", "GPT-5.4"), false);
  assert.equal(isUpstreamModelMismatch("gpt-5.4", ""), false);
  assert.equal(isUpstreamModelMismatch("", "gpt-5.4"), false);
});

test("dated snapshots, provider prefixes and alias suffixes still match", () => {
  assert.equal(isUpstreamModelMismatch("gpt-5", "gpt-5-2025-08-07"), false);
  assert.equal(isUpstreamModelMismatch("claude-sonnet-4-5", "claude-sonnet-4-5-20250929"), false);
  assert.equal(isUpstreamModelMismatch("gpt-5.4", "openai/gpt-5.4"), false);
  assert.equal(isUpstreamModelMismatch("gpt-5.4-openai-compact", "gpt-5.4"), false);
  assert.equal(isUpstreamModelMismatch("claude-3-5-sonnet-latest", "claude-3-5-sonnet-20241022"), false);
  assert.equal(isUpstreamModelMismatch("claude-sonnet-4-5[1m]", "claude-sonnet-4-5-20250929"), false);
});

test("a different model is a mismatch", () => {
  assert.equal(isUpstreamModelMismatch("gpt-5.4", "gpt-5.4-mini"), true);
  assert.equal(isUpstreamModelMismatch("gpt-5.4", "gpt-4o"), true);
  assert.equal(isUpstreamModelMismatch("claude-opus-5", "claude-sonnet-5"), true);
});
