#!/usr/bin/env node
// github — a reference turntable plugin connector for the GitHub REST API,
// built on the Node.js SDK (no dependencies, no build step; Node 18+ for fetch).
//
// Authentication resolves in order: a `token` option (use an ${ENV_VAR}
// reference in the config), the GITHUB_TOKEN / GH_TOKEN environment variables,
// then `gh auth token` — so with the GitHub CLI logged in, zero configuration
// is needed. Unauthenticated works for public data at low rate limits.
//
// Datasets:
//   repos    repositories of `owner` (user or org; default: the token's user)
//            name, full_name, private, fork, archived, stars, forks,
//            open_issues, language, default_branch, size_kb, pushed_at, created_at
//   issues   issues of `repo` — excludes PRs
//            repo, number, title, state, author, labels, comments, created_at,
//            updated_at, closed_at
//   prs      pull requests of `repo`
//            repo, number, title, state, author, draft, base, head, created_at,
//            merged_at, closed_at
//   runs     Actions workflow runs of `repo` — great time-series data
//            repo, id, workflow, run_number, event, status, conclusion, branch,
//            sha, actor, created_at, updated_at, duration_s
//
// issues/prs/runs require `repo` — one "owner/name", or several comma-separated
// — and tag every row with its repo, the JOIN key back to repos.full_name:
//
//   SELECT r.name, COUNT(p.number) AS open_prs
//   FROM repos r LEFT JOIN prs p ON p.repo = r.full_name AND p.state = 'open'
//   GROUP BY r.name
//
// Options: repo, owner, token, state (issues/prs: open|closed|all, default all),
// max_pages (100 rows per page per repo, default 5), api_url (GitHub Enterprise).
//
//   sources:
//     gh:
//       connector: plugin
//       command: ["node", "./examples/plugins/github/github.mjs"]
//       options: { dataset: "*", repo: "undefinedopcode/turntable" }
//
//   SELECT workflow, conclusion, AVG(duration_s) FROM runs GROUP BY 1, 2
import { execFileSync } from "node:child_process";
import { serve } from "../../../sdk/node/ttplugin.js";

let cachedToken;
function token(options) {
  if (cachedToken !== undefined) return cachedToken;
  cachedToken =
    options.token ||
    process.env.GITHUB_TOKEN ||
    process.env.GH_TOKEN ||
    ghToken() ||
    null;
  return cachedToken;
}

function ghToken() {
  try {
    return execFileSync("gh", ["auth", "token"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch {
    return null; // gh not installed or not logged in
  }
}

async function api(path, options, params = {}) {
  const base = (options.api_url || "https://api.github.com").replace(/\/+$/, "");
  const maxPages = Number(options.max_pages) || 5;
  const out = [];
  for (let page = 1; page <= maxPages; page++) {
    const q = new URLSearchParams({ ...params, per_page: "100", page: String(page) });
    const headers = {
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      "User-Agent": "turntable-github-plugin",
    };
    const t = token(options);
    if (t) headers.Authorization = `Bearer ${t}`;
    const resp = await fetch(`${base}${path}?${q}`, { headers });
    if (!resp.ok) {
      const body = (await resp.text()).slice(0, 200);
      throw new Error(`GitHub ${path}: HTTP ${resp.status}: ${body}`);
    }
    const data = await resp.json();
    const items = Array.isArray(data) ? data : (data.workflow_runs ?? []);
    out.push(...items);
    if (items.length < 100) break;
  }
  return out;
}

// needRepos parses the repo option: one or more comma-separated "owner/name"
// entries. issues/prs/runs fetch each and tag every row with its repo — the
// join key back to repos.full_name.
function needRepos(options) {
  const repos = String(options.repo ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
  if (repos.length === 0 || repos.some((r) => !r.includes("/"))) {
    throw new Error('this dataset needs a repo option like "owner/name" (comma-separate several)');
  }
  return repos;
}

// perRepo fetches rows for each configured repo and prefixes each row with the
// repo's full name.
async function perRepo(options, fetchOne) {
  const out = [];
  for (const repo of needRepos(options)) {
    for (const row of await fetchOne(repo)) {
      out.push([repo, ...row]);
    }
  }
  return out;
}

const repoCol = { name: "repo", type: "string" }; // joins to repos.full_name

const date = (s) => (s ? new Date(s) : null);

serve({
  name: "github",
  datasets: {
    repos: {
      columns: [
        { name: "name", type: "string" },
        { name: "full_name", type: "string" },
        { name: "private", type: "bool" },
        { name: "fork", type: "bool" },
        { name: "archived", type: "bool" },
        { name: "stars", type: "int" },
        { name: "forks", type: "int" },
        { name: "open_issues", type: "int" },
        { name: "language", type: "string", nullable: true },
        { name: "default_branch", type: "string" },
        { name: "size_kb", type: "int" },
        { name: "pushed_at", type: "time", nullable: true },
        { name: "created_at", type: "time", nullable: true },
      ],
      rows: async (req) => {
        const owner = req.options.owner;
        // The authenticated /user/repos includes private repos; /users/{x}/repos
        // is public-only.
        const path = owner ? `/users/${owner}/repos` : "/user/repos";
        const repos = await api(path, req.options, { sort: "pushed" });
        return repos.map((r) => [
          r.name, r.full_name, r.private, r.fork, r.archived,
          r.stargazers_count, r.forks_count, r.open_issues_count,
          r.language, r.default_branch, r.size, date(r.pushed_at), date(r.created_at),
        ]);
      },
    },
    issues: {
      columns: [
        repoCol,
        { name: "number", type: "int" },
        { name: "title", type: "string" },
        { name: "state", type: "string" },
        { name: "author", type: "string", nullable: true },
        { name: "labels", type: "string", nullable: true },
        { name: "comments", type: "int" },
        { name: "created_at", type: "time", nullable: true },
        { name: "updated_at", type: "time", nullable: true },
        { name: "closed_at", type: "time", nullable: true },
      ],
      rows: (req) =>
        perRepo(req.options, async (repo) => {
          const issues = await api(`/repos/${repo}/issues`, req.options, {
            state: req.options.state || "all",
          });
          return issues
            .filter((i) => !i.pull_request) // the issues API interleaves PRs
            .map((i) => [
              i.number, i.title, i.state, i.user?.login ?? null,
              i.labels?.map((l) => l.name).join(",") || null,
              i.comments, date(i.created_at), date(i.updated_at), date(i.closed_at),
            ]);
        }),
    },
    prs: {
      columns: [
        repoCol,
        { name: "number", type: "int" },
        { name: "title", type: "string" },
        { name: "state", type: "string" },
        { name: "author", type: "string", nullable: true },
        { name: "draft", type: "bool" },
        { name: "base", type: "string" },
        { name: "head", type: "string" },
        { name: "created_at", type: "time", nullable: true },
        { name: "merged_at", type: "time", nullable: true },
        { name: "closed_at", type: "time", nullable: true },
      ],
      rows: (req) =>
        perRepo(req.options, async (repo) => {
          const prs = await api(`/repos/${repo}/pulls`, req.options, {
            state: req.options.state || "all",
          });
          return prs.map((p) => [
            p.number, p.title, p.state, p.user?.login ?? null, !!p.draft,
            p.base?.ref, p.head?.ref, date(p.created_at), date(p.merged_at), date(p.closed_at),
          ]);
        }),
    },
    runs: {
      columns: [
        repoCol,
        { name: "id", type: "int" },
        { name: "workflow", type: "string" },
        { name: "run_number", type: "int" },
        { name: "event", type: "string" },
        { name: "status", type: "string" },
        { name: "conclusion", type: "string", nullable: true },
        { name: "branch", type: "string", nullable: true },
        { name: "sha", type: "string" },
        { name: "actor", type: "string", nullable: true },
        { name: "created_at", type: "time", nullable: true },
        { name: "updated_at", type: "time", nullable: true },
        { name: "duration_s", type: "int", nullable: true },
      ],
      rows: (req) =>
        perRepo(req.options, async (repo) => {
          const runs = await api(`/repos/${repo}/actions/runs`, req.options);
          return runs.map((r) => {
            const start = date(r.run_started_at ?? r.created_at);
            const end = date(r.updated_at);
            const duration =
              start && end && r.status === "completed"
                ? Math.round((end - start) / 1000)
                : null;
            return [
              r.id, r.name, r.run_number, r.event, r.status, r.conclusion,
              r.head_branch, (r.head_sha ?? "").slice(0, 8), r.actor?.login ?? null,
              date(r.created_at), date(r.updated_at), duration,
            ];
          });
        }),
    },
  },
});
