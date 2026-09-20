import { useTranslation } from "react-i18next";
import type { AccountRow } from "../types";
import {
  estimateLongUsageWindowUSD,
  formatLongUsageWindowLabel,
} from "../lib/usageFormat";

export default function AccountQuotaEstimate({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const estimate = estimateLongUsageWindowUSD(account);
  if (estimate === null) return null;

  return (
    <span
      className="account-usage-estimate block min-w-0 max-w-full whitespace-normal font-sans text-[11px] leading-4 font-normal text-muted-foreground tabular-nums [overflow-wrap:anywhere]"
      title={t("accounts.usageQuotaEstimateHint", {
        window: formatLongUsageWindowLabel(account),
      })}
    >
      {t("accounts.usageQuotaEstimate", { amount: estimate.toFixed(2) })}
    </span>
  );
}
