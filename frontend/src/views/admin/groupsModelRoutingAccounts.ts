type AbortSignalLike = AbortSignal | undefined;

export interface ModelRoutingAccountCandidate {
  id: number;
  name: string;
  platform?: string;
  type?: string;
}

interface AccountListResponse {
  items: ModelRoutingAccountCandidate[];
}

export type AccountListFn = (
  page: number,
  pageSize: number,
  filters: Record<string, string>,
  options: { signal?: AbortSignalLike },
) => Promise<AccountListResponse>;

export async function fetchModelRoutingAccountCandidates(
  keyword: string,
  signal: AbortSignalLike,
  listAccounts: AccountListFn,
): Promise<ModelRoutingAccountCandidate[]> {
  const [anthropicAccounts, codexAccounts] = await Promise.all([
    listAccounts(
      1,
      20,
      {
        search: keyword,
        platform: "anthropic",
      },
      { signal },
    ),
    listAccounts(
      1,
      20,
      {
        search: keyword,
        platform: "openai",
        type: "oauth",
      },
      { signal },
    ),
  ]);

  const seen = new Set<number>();
  const candidates: ModelRoutingAccountCandidate[] = [];

  for (const account of [...anthropicAccounts.items, ...codexAccounts.items]) {
    if (seen.has(account.id)) {
      continue;
    }
    seen.add(account.id);
    candidates.push({
      id: account.id,
      name: account.name,
      platform: account.platform,
      type: account.type,
    });
  }

  return candidates;
}

export function formatModelRoutingAccountLabel(
  account: ModelRoutingAccountCandidate,
): string {
  if (account.platform === "openai" && account.type === "oauth") {
    return `${account.name} · Codex / OpenAI OAuth`;
  }
  if (account.platform === "anthropic") {
    return `${account.name} · Anthropic`;
  }
  if (account.platform) {
    return `${account.name} · ${account.platform}`;
  }
  return account.name;
}
