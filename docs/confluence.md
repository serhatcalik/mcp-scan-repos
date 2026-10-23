# Confluence Publication

Workers can publish completed, validated documents to Confluence independently of Notion. No new pipeline or LLM generation is needed to publish an existing job. Credentials are operator-owned and never enter evidence, prompts, publication records, or API/MCP configuration.

## Cloud Setup

Use a dedicated Atlassian account with permission to view the destination space and parent page and create child pages. Create an account API token and store it in an existing Kubernetes Secret with a `token` key. Cloud authentication uses account email plus API token, not the GitLab CI token. The example uses an unscoped account token and your site's `/wiki` base URL. See [Atlassian basic authentication](https://developer.atlassian.com/cloud/confluence/basic-auth-for-rest-apis/).

```yaml
confluence:
  enabled: true
  baseURL: https://YOUR-SITE.atlassian.net/wiki
  edition: cloud
  username: docs@example.com
  tokenSecretName: confluence-integration
  tokenSecretKey: token
  mermaidMacro: ""
  mermaidBodyParameter: ""
  mermaidApp: ""
  mermaidExtensionKey: ""
  targets:
    - group_id: "11527771"
      space_id: "YOUR_NUMERIC_SPACE_ID"
      parent_page_id: "YOUR_NUMERIC_PARENT_PAGE_ID"
      enabled: true
```

Replace the space/page placeholders with numeric IDs. The space ID is not the space key shown in the browser URL. An authenticated `GET /wiki/api/v2/pages/PARENT_PAGE_ID?body-format=storage` returns the page's `spaceId`; the publisher verifies that the parent belongs to this space. See the [Cloud page API](https://developer.atlassian.com/cloud/confluence/rest/v2/api-group-page/).

For GitHub, use `group_id: "github:OWNER_ID"` with the numeric owner/user/organization ID, for example `github:101`. Space and parent page IDs are still ordinary Confluence IDs. Append GitHub entries to the existing target list to preserve GitLab publication, and deploy a worker containing GitHub publication-target validation before enabling them. See [the combined GitHub example](github-actions.md#publish-to-notion-and-confluence).

Only the worker mounts the Secret and configuration. Configuration changes trigger worker rollout through a checksum annotation. Build/deploy the new worker image and sync Helm values. Automatic migrations create the Confluence destination and page-mapping tables. All worker replicas must use the same settings.

The configuration decoder rejects unknown fields. When adding new settings such as `mermaid_app` or `mermaid_extension_key`, the running worker must include the corresponding code, not only the updated ConfigMap. A rollout restart with an older image still rejects those fields. Check the deployment image tag and wait for the pipeline to build/push the new commit's image and update the chart tag before syncing. Parse diagnostics distinguish unknown fields, malformed JSON, incorrect value types, and trailing content without logging raw configuration.

Confluence remains disabled by default. Notion and Confluence can both be enabled. Workers require approved HTTPS egress to the configured Confluence host; restrictive NetworkPolicy CIDRs must allow that destination. TLS certificates are verified, redirects are not followed, and requests have a 30-second timeout.

## Formatting And Mermaid

The publisher parses Markdown with Goldmark and emits Confluence storage-format headings, tables, lists, quotes, inline emphasis, links, citations, and code macros. It does not place an entire Markdown file in a plain-text page. Raw HTML is escaped, unsafe links are not activated, and remote images are not imported or fetched. Diagrams are never executed locally.

Every diagram is one complete macro body, including line breaks and blank lines. Visual Mermaid rendering requires the installed app's actual storage-format contract; a display name such as "Mermaid Chart" does not identify that contract. Inspect a working test page:

```bash
curl --silent --show-error --fail \
  --user "$CONFLUENCE_EMAIL:$CONFLUENCE_API_TOKEN" \
  "https://YOUR-SITE.atlassian.net/wiki/api/v2/pages/TEST_PAGE_ID?body-format=storage" |
  jq -r '.body.storage.value'
```

If the working diagram uses a plain-text macro body like this:

```xml
<ac:structured-macro ac:name="YOUR-MACRO-NAME">
  <ac:plain-text-body><![CDATA[flowchart LR
    A --> B
  ]]></ac:plain-text-body>
</ac:structured-macro>
```

Set `mermaidMacro: YOUR-MACRO-NAME` and leave `mermaidBodyParameter` empty. If the app instead accepts the diagram in an `ac:parameter`, set `mermaidBodyParameter` to that parameter's exact name. XML text and CDATA terminators are escaped safely. See [Confluence storage format](https://confluence.atlassian.com/doc/confluence-storage-format-790796544.html).

The legacy macro settings support plain-text-body or single-text-parameter macros. Apps requiring additional parameters, rich bodies, or remote attachment IDs need a specific adapter; do not guess their macro name. With no app or macro configured, diagrams use one complete Confluence code macro rather than a broken/unknown Mermaid macro. This preserves source but does not visually render the diagram. Configure the app before first publication if visual rendering is required. Use the app's strict security options for untrusted diagram source.

### Narva Software Mermaid

Narva's Cloud app uses a Forge ADF extension rather than an `ac:structured-macro` named `mermaid`. Its diagram is in the `guest-params.code` field. The adapter is based on an inspected working page, not an inferred macro name. Set:

```yaml
confluence:
  mermaidApp: narva
  mermaidExtensionKey: "336440d6-2d95-4275-833f-6351e86634e3/e5743d37-53d6-4daa-a3a1-acca68790e69/static/mermaid-diagram"
  mermaidMacro: ""
  mermaidBodyParameter: ""
```

The extension key identifies the app and its deployed environment, not a GitLab project or Confluence page. Verify it against `extension-key` in a working page's storage body if your installation differs. Narva mode is Cloud-only and rejects incompatible or ambiguous legacy macro settings. The chart's configured installation uses this adapter; a different deployment can leave `mermaidApp` and `mermaidExtensionKey` empty to retain the code fallback.

The publisher emits the primary ADF extension and its fallback, escapes the complete source into `guest-params.code`, and uses stable block-local IDs across retries. It does not copy the example's user, workspace, cloud, page, or version IDs. For this specific Narva app, recovery ignores host-injected `embedded-macro-context` and local IDs, while retaining checks on app/environment identity, guest parameters, primary/fallback diagram source, and other page content. Unknown Forge apps are not given this normalization. See the [Forge macro reference](https://developer.atlassian.com/platform/forge/manifest-reference/modules/macro/).

Rebuild/deploy the worker and sync configuration, then publish a new documentation generation. Existing snapshots are immutable and will not be converted from code blocks to Narva extensions by retrying. In particular, a managed architecture page where a test macro, whiteboard, or custom text was manually added remains protected as edited content. Do not delete its mappings or overwrite it to force migration. Mock tests verify the emitted storage contract and recovery; the first live generation still needs visual confirmation in Confluence.

## Page Structure And Recovery

Each group maps to a space and parent page. A named project page contains generation snapshots, each with Technical Paper, Infrastructure Paper, Application Architecture, Infrastructure Architecture, and Dependency Diagram child pages. The project name appears only on the project page; new snapshot and document titles do not repeat it. For example, `Documentation - 1.0.0` contains `Technical Paper - 1.0.0` and `Application Architecture - 1.0.0`. Child titles lead with their document kind; the version/date and optional revision suffix ensure space-wide uniqueness. The title style is recorded before page creation so older publications keep their original child names during retries. Job/project IDs remain in identity paragraphs, not the snapshot title.

New snapshots prefer the verified application version: `Documentation - 1.0.0`. The analyzer reads root `VERSION`/`version.txt` declarations and root `package.json.version` or the project's own `pom.xml` version. Explicit version files take precedence; conflicting declarations, unresolved build expressions, unsupported formats, or absent versions fall back to completion date/time in UTC. Parent/dependency versions, nested component versions, and repository code execution are never used to guess the application version. Versions are bounded numeric release labels, including optional `v`, prerelease and build suffixes. Version selection uses stored verified evidence, not LLM guesses.

For Go releases versioned by Git tags, the updated CI template captures tags pointing at the exact uploaded commit. These CI-declared tags are preferred after explicit `VERSION`/`version.txt` files and before manifest/source fallbacks. The worker verifies the declaration's project/commit identity but does not query GitLab or independently authenticate the tag mapping. See [Git tag capture](gitlab-pipelines.md#go-versions-from-git-tags). Literal package-level Go `Version`/`AppVersion`/`ApplicationVersion` declarations (including lower-case forms) are supported as a fallback in root Go files, `cmd/*/main.go`, and conventional version/buildinfo files. `go.mod` language/toolchain versions, dependency versions, function-local variables, computed expressions, and runtime `ldflags` overrides are not application-version evidence. Conflicting source declarations still fall back to dates.

Confluence requires titles to be unique across the space, even under different parents. Repeated version or date labels become `Documentation - 1.0.0 - Revision 2`, then `Revision 3`; allocation is shared across projects/parents in the same site and space. Other sites/spaces remain independent. The chosen label and final title are persisted before remote writes and remain stable across retries. Older publication records retain their project-prefixed titles, including partial publications, so retries do not trigger title mismatches. Published pages are not retroactively renamed. Historical analyses without version evidence still use dates. New source analysis is needed to capture newly supported version declarations. See [Confluence naming rules](https://support.atlassian.com/confluence-cloud/docs/move-copy-and-hide-pages/).

The publication queue is PostgreSQL-backed with a separate session advisory lock and shared request cooldown. Attempts run separately from generation with a five-minute deadline and at most ten attempts. HTTP 429, transient errors, and interrupted attempts retry with backoff; `Retry-After` sets the shared cooldown. Authentication/permission/request errors and edited-page mismatches stop for operator correction. Raw remote error messages are never stored or logged.

Page content is immutable: the publisher never updates or deletes an existing page. After an ambiguous create, it searches the exact title and verifies space, parent, and complete body before recovering the ID. Human edits, unrelated title collisions, and moved pages are not overwritten. Confluence-generated macro IDs/schema-version attributes, stripped Goldmark footnote fragment-navigation attributes, and equivalent non-breaking-space entities are ignored; citation numbers, source notes, meaningful text, formatting, ordinary/external links, code whitespace, and macro parameters remain checked. Entity normalization occurs during XML parsing, not by unescaping markup or diagram CDATA. A deleted page or renamed project requires operator reconciliation, not deletion of mappings to force a replacement.

First enablement publishes only jobs completed after destination activation. Re-enabling the same destination catches up disabled-period jobs. Changing site, edition, space, or parent starts a separate destination with a new activation boundary. Omitted/disabled targets stop publication without deleting pages. Changing the renderer or Mermaid settings does not rewrite existing snapshots; use a new generation for different formatting.

## Publish An Existing Job

After the updated worker has registered your enabled target, enqueue just this completed job:

```sql
insert into publication_records (job_id, target, status)
select j.id, 'confluence:' || t.destination_key, 'queued'
from documentation_jobs j
join confluence_targets t on t.group_id = j.group_id and t.enabled
where j.id = '57e0c15c987f93a13b2d68d0ab54c944'
  and j.status = 'completed'
on conflict (job_id, target) do nothing
returning job_id, target, status;
```

Check status in the existing authorized API publication endpoint or MCP project/document queries, or directly in PostgreSQL:

```sql
select target, status, attempt_count, details->>'error' as error,
       details->>'project_url' as project_url,
       details->>'snapshot_url' as snapshot_url
from publication_records
where job_id = '57e0c15c987f93a13b2d68d0ab54c944'
  and target like 'confluence:%';
```

After correcting a failed publication's configuration/permissions:

```sql
update publication_records
set status = 'retrying', attempt_count = 0,
    next_run_at = now(), updated_at = now()
where job_id = '57e0c15c987f93a13b2d68d0ab54c944'
  and target like 'confluence:%'
  and status = 'failed';
```

Preserve `confluence_pages` mappings. Retry does not regenerate documents or overwrite edited pages. Logs include `Confluence publication attempt finished` and safe HTTP status/operation diagnostics.

An edited-page rejection also identifies the document kind, numeric page ID, and a fixed reason: `space_mismatch`, `parent_mismatch`, `title_mismatch`, or `body_mismatch`. For example, `Confluence technical page: managed Confluence page differs; page_id=123456; reason=body_mismatch; refusing to overwrite`. Titles and document bodies are never included in the error. A `body_mismatch` can indicate a real edit or a storage transformation not supported by our comparison; inspect the page before changing normalization. A newly created page can fail this check before its ID is mapped, so an absent document mapping does not prove the remote page was never created. Preserve existing mappings and pages while investigating.

## Data Center

The same storage renderer also supports the Data Center content API. Set `edition: data-center`, use an HTTPS base URL including any context path, replace `space_id` with `space_key`, and leave `username` empty for personal-access-token Bearer authentication. Supplying a username uses Basic authentication instead. The installed Mermaid app must still accept one of the supported macro contracts. See the [Data Center API](https://developer.atlassian.com/server/confluence/rest/latest/).

## Verification

Unit tests cover Markdown formatting, one-block Mermaid preservation, XML injection/CDATA safety, sensitive error handling, redirects, edition-specific authentication/payloads, ambiguous-create recovery, destination isolation, and edited-page protection. Helm tests verify worker-only configuration and credential mounts. With `TEST_DATABASE_URL`, integration tests exercise migrations, durable retries, page mappings, activation boundaries, group isolation, and replica locks. Live Confluence/app compatibility requires a private test page and real credentials; mocks cannot establish it.
