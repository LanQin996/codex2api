import assert from "node:assert/strict";
import test from "node:test";

import {
  estimateLongUsageWindowUSD,
  getAccountStatusBadgeStatus,
  isOfficialCostHiddenAccount,
  isOfficialCostTooNew,
  isUnsampledQuotaAccount,
  needsOfficialCostReload,
  needsUsageReload,
  officialUsdFromDailyItems,
  officialUsdValue,
  supportsOfficialUsage,
  isWorkspaceCreditHardStop,
} from "./usageFormat.ts";

const estimateNow = Date.parse("2026-09-20T12:00:00Z");
const estimateAccount = {
  usage_percent_7d: 14,
  billed_7d: 87.59,
  reset_7d_at: "2026-09-27T12:00:00Z",
};

test("long-window estimate extrapolates current-cycle account cost to full quota", () => {
  assert.equal(estimateLongUsageWindowUSD(estimateAccount, estimateNow).toFixed(2), "625.64");
  assert.equal(
    estimateLongUsageWindowUSD({ ...estimateAccount, usage_percent_7d: 100 }, estimateNow),
    87.59,
  );
  assert.equal(
    estimateLongUsageWindowUSD({ ...estimateAccount, usage_percent_7d: 12.5, billed_7d: 10 }, estimateNow),
    80,
  );
  assert.equal(
    estimateLongUsageWindowUSD({
      ...estimateAccount,
      usage_7d_detail: { account_billed: 500, user_billed: 1000 },
      official_usd: 999,
      official_usd_7d: 999,
    }, estimateNow).toFixed(2),
    "625.64",
  );
});

test("long-window estimate hides missing, zero, invalid and overflowing samples", () => {
  assert.equal(estimateLongUsageWindowUSD({}, estimateNow), null);
  for (const percent of [undefined, null, 0, -1, 101, NaN, Infinity, "14"]) {
    assert.equal(
      estimateLongUsageWindowUSD({ ...estimateAccount, usage_percent_7d: percent }, estimateNow),
      null,
    );
  }
  for (const billed of [undefined, null, 0, -1, NaN, Infinity, "87.59"]) {
    assert.equal(
      estimateLongUsageWindowUSD({ ...estimateAccount, billed_7d: billed }, estimateNow),
      null,
    );
  }
  assert.equal(
    estimateLongUsageWindowUSD({ ...estimateAccount, billed_7d: Number.MAX_VALUE }, estimateNow),
    null,
  );
});

test("long-window estimate requires a current reset window and preserves monthly windows", () => {
  for (const reset of [undefined, null, "", "invalid", "2026-09-19T12:00:00Z", "2026-09-20T12:00:00Z"]) {
    assert.equal(
      estimateLongUsageWindowUSD({ ...estimateAccount, reset_7d_at: reset }, estimateNow),
      null,
    );
  }
  assert.equal(
    estimateLongUsageWindowUSD({
      ...estimateAccount,
      usage_window_7d_kind: "monthly",
      usage_window_7d_seconds: 30 * 86400,
      reset_7d_at: "2026-10-01T12:00:00Z",
    }, estimateNow).toFixed(2),
    "625.64",
  );
});

test("usage reload accepts either optional usage window as sampled", () => {
  assert.equal(needsUsageReload({ status: "active" }), true);
  assert.equal(
    needsUsageReload({ status: "active", usage_percent_5h: 12 }),
    false,
  );
  assert.equal(
    needsUsageReload({ status: "ready", usage_percent_7d: 34 }),
    false,
  );
});

test("usage reload skips accounts that cannot be sampled", () => {
  assert.equal(needsUsageReload({ status: "unauthorized" }), false);
});

test("Claude usage probe without quota headers still counts as sampled", () => {
  const sampled = {
    status: "active",
    claude_api: true,
    claude_usage_probe_at: "2026-08-29T05:00:00Z",
    claude_usage_probe_error: "",
  };
  assert.equal(needsUsageReload(sampled), false);
  assert.equal(isUnsampledQuotaAccount(sampled), false);
  assert.equal(getAccountStatusBadgeStatus(sampled), "active");
});

test("Claude probe failures remain unsampled and are not eligible for OpenAI billing", () => {
  const failed = {
    status: "active",
    claude_api: true,
    claude_usage_probe_at: "2026-08-29T05:00:00Z",
    claude_usage_probe_error: "upstream timeout",
  };
  assert.equal(needsUsageReload(failed), true);
  assert.equal(isUnsampledQuotaAccount(failed), true);
  assert.equal(supportsOfficialUsage(failed), false);
  assert.equal(needsOfficialCostReload(failed), false);
});

test("unsampled quota accounts are not treated as available", () => {
  assert.equal(isUnsampledQuotaAccount({ status: "active" }), true);
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", usage_percent_5h: 8 }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "unauthorized" }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", grok_api: true }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", openai_responses_api: true }),
    false,
  );
  assert.equal(getAccountStatusBadgeStatus({ status: "active" }), "unsampled");
  assert.equal(
    getAccountStatusBadgeStatus({ status: "active", usage_percent_7d: 12 }),
    "active",
  );
  assert.equal(
    getAccountStatusBadgeStatus({ status: "rate_limited" }),
    "rate_limited",
  );
  assert.equal(
    getAccountStatusBadgeStatus({ status: "overload_paused" }),
    "active",
  );
});

test("official cost reload only retries Codex accounts missing the snapshot", () => {
  assert.equal(needsOfficialCostReload({}), true);
  assert.equal(needsOfficialCostReload({ official_usd: 0 }), false);
  assert.equal(needsOfficialCostReload({ official_usd_7d: 0 }), false);
  assert.equal(needsOfficialCostReload({ official_usd_7d: 12.5 }), false);
  assert.equal(needsOfficialCostReload({ openai_responses_api: true }), false);
  assert.equal(needsOfficialCostReload({ grok_api: true }), false);
  assert.equal(needsOfficialCostReload({ claude_api: true }), false);
  assert.equal(
    needsOfficialCostReload({ access_token_type: "codex_at" }),
    false,
  );
  assert.equal(needsOfficialCostReload({ status: "error" }), false);
  assert.equal(needsOfficialCostReload({ status: "unauthorized" }), false);
  assert.equal(
    needsOfficialCostReload({
      created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
    }),
    false,
  );
  assert.equal(
    needsOfficialCostReload({
      created_at: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
    }),
    true,
  );
  assert.equal(supportsOfficialUsage({}), true);
  assert.equal(supportsOfficialUsage({ access_token_type: "codex_at" }), false);
  assert.equal(supportsOfficialUsage({ access_token_type: " CODEX_AT " }), false);
  assert.equal(supportsOfficialUsage({ access_token_type: "codex_at", chatgpt_account_id: "acc_123" }), true);
  assert.equal(supportsOfficialUsage({ access_token_type: "codex_at", effective_workspace_id: "acc_456" }), true);
  assert.equal(supportsOfficialUsage({ access_token_type: "codex_at", chatgpt_account_id: "", effective_workspace_id: "" }), false);
  assert.equal(supportsOfficialUsage({ openai_responses_api: true }), false);
  assert.equal(supportsOfficialUsage({ grok_api: true }), false);
  assert.equal(supportsOfficialUsage({ claude_api: true }), false);

  assert.equal(isWorkspaceCreditHardStop({ credits_spend_control_reached: true }), true);
  assert.equal(isWorkspaceCreditHardStop({ credits_rate_limit_reached_type: "workspace_member_credits_depleted" }), true);
  assert.equal(isWorkspaceCreditHardStop({ credits_rate_limit_reached_type: "WORKSPACE_OWNER_USAGE_LIMIT_REACHED" }), true);
  assert.equal(isWorkspaceCreditHardStop({ credits_rate_limit_reached_type: "other_limit" }), false);
  assert.equal(isWorkspaceCreditHardStop({}), false);
  assert.equal(isOfficialCostHiddenAccount({ status: "error" }), true);
  assert.equal(isOfficialCostHiddenAccount({ status: "active" }), false);
  assert.equal(
    isOfficialCostTooNew({
      created_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
    }),
    true,
  );
});

test("official cost reload stops once the backend reports a completed sync", () => {
  // 同步成功但上游没有数据(官方统计滞后):继续重拉不会有结果,必须停。
  assert.equal(needsOfficialCostReload({ official_usage_synced: true }), false);
  assert.equal(
    needsOfficialCostReload({ official_usage_synced: false }),
    true,
  );
});

test("official billed badge prefers official_usd over the 7d alias", () => {
  assert.equal(officialUsdValue({}), null);
  assert.equal(officialUsdValue({ official_usd_7d: 1.5 }), 1.5);
  assert.equal(officialUsdValue({ official_usd: 9, official_usd_7d: 1.5 }), 9);
});

test("official billed badge sums all synced days unless a window is requested", () => {
  assert.equal(officialUsdFromDailyItems([]), null);
  assert.equal(
    officialUsdFromDailyItems([
      { day: "2026-08-20", usd: 10 },
      { day: "2026-08-25", usd: 1.25 },
      { day: "2026-08-26", usd: 2.5 },
      { day: "2026-08-27", usd: 3 },
    ]),
    16.75,
  );
  assert.equal(
    officialUsdFromDailyItems(
      [
        { day: "2026-08-20", usd: 10 },
        { day: "2026-08-26", usd: 2 },
        { day: "2026-08-27", usd: 3 },
      ],
      2,
    ),
    5,
  );
});
