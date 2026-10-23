# Notion publishing

Workers publish validated documentation under a Notion parent page configured for each GitLab group or GitHub owner. Share the parent URL with your team. Notion reader access and connection access are separate: this feature does not make pages public or change sharing permissions.

## Structure

```text
Shared documentation parent page
  serhatcalik/diyetapi                 <- stable project URL; human notes can live here
    Documentation - 2026-10-13 00:08:00 UTC
      Technical Paper
      Infrastructure Paper
      Application Architecture
      Infrastructure Architecture
      Dependency Diagram
    Documentation - 2026-10-13 01:15:00 UTC
      ...
```

Snapshot metadata includes project name/ID, commit, main branch, ingestion timestamp and job ID. Papers retain evidence citations and verified/inferred/unavailable labels. Markdown is parsed into native Notion headings, paragraphs, lists, checkboxes, quotes, dividers, and tables. Bold, italic, strikethrough, inline code, and safe absolute links use rich-text formatting rather than literal Markdown syntax. Footnote citations become numbered source notes. Full evidence, metadata, reference artifacts and downloadable bundles remain in platform storage.

Project titles use the GitLab namespace/repository path, falling back to a numeric title only when no name is available. New snapshot titles prefer a verified application version, falling back to the job's completed-at timestamp in UTC, not the worker's current time. Labels are persisted so retries keep the same name. New project/snapshot pages contain an identity paragraph created atomically with the page. Equal visible titles or completion timestamps cannot cause one project's/job's page to be reused for another.

Existing mapped pages with the exact legacy `GitLab Project ID` or `Documentation [job ID]` title are renamed during publication; their IDs, URLs, child pages, and contents stay unchanged. Custom titles are preserved. Already-published records are not automatically revisited, so historical snapshot titles are not bulk-renamed. A new publication updates the shared project title and creates a dated snapshot.

Each architecture diagram is one complete Notion code block with `language: mermaid`, including original line breaks and blank lines. Long code uses multiple rich-text segments within that same block, never a separate block per line. Other fenced code retains supported language labels. Notion controls the diagram display in its UI. See the [Notion code/table block reference](https://developers.notion.com/reference/block).

The publisher uses the block API so retries can compare structured content before appending. Tables are split into groups of at most 20 rows with repeated headers; this respects nested block limits without flattening cells into pipe-delimited text. Nested lists retain their text/order but are flattened. Raw HTML and images are displayed as literal text, not active embeds or fetched assets. Unsupported/relative link targets are preserved as text. Document size, structure depth, rich-text lengths, and request payloads are bounded.

Each generation has separate pages, so a delayed generation cannot overwrite a newer one. Keep human notes on the stable project page or separate pages. The worker refuses to overwrite changed managed content. Avoid editing/renaming managed document pages during incomplete publishing.

## Setup

1. Create a parent page such as `Engineering Documentation / Backend`.
2. Create an internal Notion connection with read, insert and update content capabilities. User-information permissions are unnecessary.
3. Grant the connection access to the parent page and share the parent with your readers.
4. Copy its page ID from the URL and provision the token using your normal Secret management process.

See [Notion authorization](https://developers.notion.com/guides/get-started/authorization) and [subpages](https://www.notion.com/en-gb/help/create-a-subpage).

This release uses one connection token per worker deployment. All configured parents must be accessible to that connection. Choose separate parent pages for groups with different reader permissions. Only trusted operator configuration selects destinations; repository uploads cannot provide destinations or tokens.

## Helm

Create an existing Secret named, for example, `notion-integration`, with a `token` key. Never put the token in Git, Helm values or a ConfigMap. Configure:

```yaml
notion:
  enabled: true
  tokenSecretName: notion-integration
  tokenSecretKey: token
  targets:
    - group_id: "101"
      parent_page_id: "11111111-1111-1111-1111-111111111111"
      enabled: true
```

Replace example IDs with actual IDs. Only workers mount the token and target files. Restart workers for token rotation. Target changes update the worker checksum. Keep replicas on the same target configuration; an in-progress attempt can finish using its previous destination during rollout.

For GitHub, use `group_id: "github:OWNER_ID"` with the numeric owner/user/organization ID, for example `github:101`. GitLab groups retain their plain numeric IDs. Append GitHub entries to the existing target list to preserve GitLab publication. Deploy a worker containing GitHub publication-target validation before enabling these targets. See [the combined GitHub example](github-actions.md#publish-to-notion-and-confluence).

Workers require HTTPS egress to `api.notion.com`. The chart HTTPS policy permits it by default; restricted `networkPolicy.httpsCIDRs` must permit your approved Notion egress path.

## Local configuration

Create an operator-owned targets file:

```json
{"targets":[{"group_id":"101","parent_page_id":"11111111111111111111111111111111","enabled":true}]}
```

Store the token in a separate protected file and set:

```sh
export NOTION_ENABLED=true
export NOTION_TARGETS_FILE=/path/to/targets.json
export NOTION_TOKEN_FILE=/path/to/protected-token-file
go run ./cmd/worker
```

Defaults are `/etc/notion/targets.json` and `/etc/notion-secret/token`. Publishing is disabled by default. Targets require unique numeric group IDs and valid page IDs. Set `enabled: true` explicitly to activate a target. Unknown fields, including token fields, are rejected. Invalid enabled configuration prevents worker startup. Targets omitted from an enabled worker's configuration become disabled in PostgreSQL.

## Status and shareable URLs

Generation and publication have independent states. CI downloads remain available while Notion is unavailable. Publishing discovers completed jobs on its next worker poll.

Use an authorized GitLab OIDC request to:

```text
GET /api/v1/jobs/{job_id}/publications
```

The response contains `job_id` and a `publications` array. Each record includes `status`, `attempts`, `updated_at`, and, once pages exist, `parent_url`, `project_url` and `snapshot_url`. MCP `get_documentation_status` includes the same records. Both authorize the job before exposing destinations.

Share `parent_url` for the whole folder, `project_url` for an application, or `snapshot_url` for one generation. Readers need the access granted in Notion. No sharing permissions are changed automatically.

## Persistence and retries

Migration 004 adds targets, page mappings, queue fields and a shared cooldown. Completed jobs reconcile into uniquely keyed publication records, recovering a crash between generation completion and enqueue. A dedicated PostgreSQL session advisory lock serializes publishing across worker replicas independently of inference slots. Each attempt is bounded to five minutes.

Statuses are `queued`, `publishing`, `retrying`, `published` and `failed`. Temporary/network failures retry up to ten attempts with exponential delays. Authentication/permission/request failures stop for correction. A shared cooldown respects rate-limit `Retry-After`, with requests spaced at least 400 milliseconds apart. Interrupted work has a one-minute recovery delay. See [Notion limits and ambiguous write guidance](https://developers.notion.com/reference/request-limits).

Retries reconcile identity-bearing page titles and matching content prefixes before creating/appending missing content. Table rows are retrieved separately and compared recursively. Text, formatting, links, table dimensions/header flags, checkbox states, and code languages must match. Default response colors and harmless rich-text segmentation differences are normalized. Page mappings are durable. Notion has no general create-page idempotency key; reconciliation reduces duplicates but cannot guarantee exactly-once delivery under every remote consistency failure. Duplicate managed titles or changed content require operator reconciliation. Remote response bodies and token-bearing transport errors are never logged. Redirects are refused, and publishing rechecks content redaction.

Deploy the updated worker and create a new documentation job to publish native formatting (for example, authorized MCP regeneration with a new request ID, or a new source commit through CI). Published legacy snapshots are not rewritten. A retry against old literal Markdown or line-by-line code blocks is intentionally rejected as a layout mismatch rather than deleting/replacing existing pages. Re-running CI for an already-completed identical job does not create a new snapshot.

The updated worker logs `Notion publisher started` with `renderer=native-blocks-v1`. If a new page still shows literal pipe tables and one plain-text code block per diagram line, inspect the worker image tag and confirm the renderer in its startup log. New source files in Git do not change a previously built commit-SHA image or an already-published Notion snapshot.

Publication errors include the HTTP status, operation (such as `create_page`, `append_blocks`, or `update_page_title`), known Notion error code, and recognized validation field paths. Raw error messages may echo document content or credentials, so only allowlisted codes and schema paths are retained. The same safe error is stored in `publication_records.details.error`, returned by publication-status queries, and logged with the attempt result.

When an older worker stored only `Notion API returned HTTP 400`, deploy the diagnostic update and retry that publication to capture the operation/fields. Do not regenerate documents or delete page mappings just to diagnose a publication error:

```sql
update publication_records
set status = 'retrying', attempt_count = 0, next_run_at = now(), updated_at = now()
where job_id = 'JOB_ID' and status = 'failed';
```

A permanent rejection will stop again with the new diagnostics. Correct the indicated field/permission/configuration before retrying further. Table/code validation failures should not be worked around by flattening the page to plain text.

First enablement publishes only jobs completed after target activation. Changing the parent starts the new destination from that point. Re-enabling the same parent retains its original activation date and catches up disabled-period jobs. Existing pages are never deleted by disabling publishing.

For an intentional historical backfill, a database operator can set an earlier activation date:

```sql
update notion_targets set activated_at = timestamptz '2026-10-01 00:00:00+00'
where group_id = '101';
```

After correcting a failed publication's permissions/configuration:

```sql
update publication_records
set status = 'retrying', attempt_count = 0, next_run_at = now(), updated_at = now()
where job_id = 'JOB_ID' and target = 'notion:PARENT_PAGE_ID' and status = 'failed';
```

Preserve page mappings for recovery. Deleted/changed managed pages require operator reconciliation first. Snapshot retention and Notion workspace capacity follow your team's operational policy.

## Version-Based Snapshot Titles

New snapshots prefer verified application versions, for example `Documentation - 1.0.0`. The analyzer reads root `VERSION`/`version.txt` files, root `package.json.version`, and the project's own root `pom.xml` version, without running repository code. Explicit version files take precedence. Missing, invalid, conflicting, or unresolved versions use the existing completion date/time in UTC; parent/dependency and nested component versions are not used. Versions come from stored evidence rather than model inference.

For Go repositories using Git tags, deploy the updated [CI template](gitlab-pipelines.md#go-versions-from-git-tags) to capture tags pointing exactly at the uploaded main commit. CI-declared tags take precedence over manifest/literal Go-source fallbacks, but explicit root version files remain authoritative. The worker validates project/commit identity without querying GitLab; it does not independently verify the remote tag mapping. Go literal declarations in root or conventional version/command files are also supported; Go toolchain versions and build-time `ldflags` are not treated as release versions.

Repeated generations at the same version get `Revision 2`, `Revision 3`, and so on, scoped by project and destination. The chosen label is frozen in the publication record before page creation, so retries cannot change it. Existing mapped snapshots retain their date-based titles and page IDs. Historical jobs whose evidence predates version extraction continue to use date labels.

## Verification

Mock tests cover native tables and inline formatting, complete Mermaid blocks, citation notes, unsafe link/image handling, ambiguous committed creates/appends, nested table recovery, formatting/language edit preservation, Unicode bounds, cancellation, safe errors and redirects. API/MCP tests cover project isolation. With `TEST_DATABASE_URL`, integration tests exercise queue persistence, replica coordination, group/historical isolation, stable URLs, snapshot history and disabled targets. Real Notion verification requires a connection token and a private test parent page.
