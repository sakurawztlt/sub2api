import { describe, expect, it, vi } from "vitest";

import {
  fetchModelRoutingAccountCandidates,
  formatModelRoutingAccountLabel,
} from "../groupsModelRoutingAccounts";

describe("groupsModelRoutingAccounts", () => {
  it("fetches anthropic accounts and openai oauth accounts for routing", async () => {
    const listAccounts = vi
      .fn()
      .mockResolvedValueOnce({
        items: [
          {
            id: 1,
            name: "Claude Pool",
            platform: "anthropic",
            type: "oauth",
          },
        ],
      })
      .mockResolvedValueOnce({
        items: [
          {
            id: 2,
            name: "Codex Pool",
            platform: "openai",
            type: "oauth",
          },
        ],
      });

    const result = await fetchModelRoutingAccountCandidates(
      "sonnet",
      undefined,
      listAccounts,
    );

    expect(listAccounts).toHaveBeenCalledTimes(2);
    expect(listAccounts).toHaveBeenNthCalledWith(
      1,
      1,
      20,
      {
        search: "sonnet",
        platform: "anthropic",
      },
      { signal: undefined },
    );
    expect(listAccounts).toHaveBeenNthCalledWith(
      2,
      1,
      20,
      {
        search: "sonnet",
        platform: "openai",
        type: "oauth",
      },
      { signal: undefined },
    );
    expect(result).toEqual([
      {
        id: 1,
        name: "Claude Pool",
        platform: "anthropic",
        type: "oauth",
      },
      {
        id: 2,
        name: "Codex Pool",
        platform: "openai",
        type: "oauth",
      },
    ]);
  });

  it("formats routing account labels with platform hints", () => {
    expect(
      formatModelRoutingAccountLabel({
        id: 1,
        name: "Claude Pool",
        platform: "anthropic",
        type: "oauth",
      }),
    ).toBe("Claude Pool · Anthropic");

    expect(
      formatModelRoutingAccountLabel({
        id: 2,
        name: "Codex Pool",
        platform: "openai",
        type: "oauth",
      }),
    ).toBe("Codex Pool · Codex / OpenAI OAuth");
  });
});
