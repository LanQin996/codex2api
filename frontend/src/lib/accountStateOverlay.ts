import type { AccountRow } from "../types";

export type AccountOverlayKind = "disabled" | "overload";

export function isDisabledAccountOverlayAccount(account: AccountRow): boolean {
  return account.enabled === false;
}

export function resolveAccountOverlayKind(
  account: AccountRow,
): AccountOverlayKind | null {
  if (isDisabledAccountOverlayAccount(account)) return "disabled";
  if (account.status === "overload_paused") return "overload";
  return null;
}

export function accountStateSurfaceClass(
  account: AccountRow,
  extraWhenActive = "",
): string {
  return resolveAccountOverlayKind(account)
    ? ` account-state-surface${extraWhenActive}`
    : "";
}

export function disabledAccountSurfaceClass(
  account: AccountRow,
  extraWhenActive = "",
): string {
  return isDisabledAccountOverlayAccount(account)
    ? ` account-state-surface${extraWhenActive}`
    : "";
}

function accountStateTableRowClassForKind(kind: AccountOverlayKind): string {
  return ` account-state-table-row account-state-table-row--${kind}`;
}

export function accountStateTableRowClass(account: AccountRow): string {
  const kind = resolveAccountOverlayKind(account);
  return kind ? accountStateTableRowClassForKind(kind) : "";
}

export function disabledAccountTableRowClass(account: AccountRow): string {
  return isDisabledAccountOverlayAccount(account)
    ? accountStateTableRowClassForKind("disabled")
    : "";
}

export function isResponsesRateLimitedAccount(account: AccountRow): boolean {
 const reasons = new Set(["responses_rate_limited", "rate_limited", "rate_limited_5h", "rate_limited_7d"]);
 return reasons.has(account.status) || reasons.has(account.cooldown_reason ?? "");
}
