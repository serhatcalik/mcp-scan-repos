# Condensed Technical and Infrastructure Papers

Documentation schema 0.6.2 includes condensed papers and a component-level application diagram. Deploy the new image for API, worker, and MCP, then rerun the application CI job or use authorized MCP regeneration. API and worker must share the new schema cache key. Old completed papers and published Confluence/Notion snapshots are not rewritten automatically. CI authentication, upload fields, and polling are unchanged.

## Application Architecture

The application diagram is an overview, not a box for every extracted declaration. Version strings, framework/module metadata, DTOs, exceptions, generic structs, model fields, and test-source declarations are not individual architecture nodes. These facts remain in the full evidence snapshot and the appropriate papers/references.

Spring components are grouped by declared Java package and explicit role: controllers/mappings, services, repositories, entities, and application entrypoints. Each node shows up to three sorted component names and a count for additional components. Packages without annotations or a recognized repository declaration are not assigned a guessed role. Spring Data repository interfaces are recognized only with an explicit known supertype and import (or a fully qualified supertype). Wildcard imports and unresolved inheritance are not guessed.

Java package/import declarations are extracted conservatively from comment/string-masked source. Arrows are created only for explicit imports that resolve to a unique analyzed repository type in a displayed component. Duplicate qualified type names across modules are treated as ambiguous. These arrows mean source imports, not calls, dependency injection, network traffic, or runtime deployment. Same-package references, wildcard/static imports, and call graphs remain unresolved unless independently supported. Grouping likewise describes declarations, not proven runtime layers.

Go package/import relationships remain visible, but per-struct inventories are omitted. Literal API routes and supported route exports are summarized into named API groups with declaration counts. Spring mapping declarations are summarized per controller package without concatenating class/method mappings into invented effective paths. Full routes/annotations remain in `references/api-reference.md`.

The overview is capped at 24 component nodes, prioritizing components with explicit relationships and using deterministic ordering for ties. Source-file/line references remain in Mermaid comments. Omitted component counts are included in comments; complete declarations remain in `evidence.json`. Java imports support diagram edges but do not add a new raw import inventory to papers or inference prompts.

## Technical Paper

The verified overview groups technology, application components, literal API routes, models, external library references, security declarations, configuration keys, and telemetry references. Duplicate package/key declarations are collapsed. Model fields, generic Service structs, standard-library imports, test helpers, CI variables, and the raw Go dependency inventory are omitted from this overview but retained in evidence.json. Imports are source references, not evidence that an integration is running.

## Infrastructure Paper

Container builds, Compose files, manifests, and CI/workflow definitions have separate sections. File-level declarations do not imply service-to-image associations, effective runtime settings, or an active production deployment. Application configuration keys and CI configuration are separate. Recognized telemetry imports and middleware are described as source references, not enabled exporters; no analyzer absence claim is printed simply because those facts were categorized as dependencies or middleware.

## Evidence and References

Both papers retain Verified Facts, Inferred Information, Unavailable Information, and Source Evidence sections. Tables show at most 12 groups per section, six declaration types per group, and six distinct values per type. Long display values are abbreviated with an explicit full-evidence notice. Omission counts are shown; no underlying snapshot items are deleted.

Short source footnotes replace repeated evidence hashes. They identify the exact source files and cited lines; evidence.json retains full identities, confidence, warnings, inventory, and all declaration details. Successful artifact bundles also include:

- `generated-docs/references/api-reference.md`: full API declarations, grouped by named API, with confidence, source locations, and IDs.
- `generated-docs/references/configuration-reference.md`: full configuration key/reference declarations and their sources; no secret values.

MCP's paper tools return the condensed persisted papers; `get_evidence` remains available for paginated supporting details. Reference Markdown files are included in the downloadable artifact bundle.

## Model Interpretation

The prompt omits repetitive model fields, package declarations, handler inventories, standard-library imports, tests, and Go dependency inventories. It deduplicates equivalent facts and prioritizes deployment declarations ahead of application keys in infrastructure prompts. Root-level literal routes receive API-group component names. Prompt records omit redundant source strings; IDs still resolve to the original snapshot and citations remain component-scoped.

The existing conservative context budget, output limit, two sequential paper requests, and validation retry limit are unchanged. Model summaries are explicitly inferred. Generic model recommendations are not published, and the prompt asks for descriptions rather than advice. Claims naming an uppercase underscore-separated setting must cite evidence containing that exact name. This blocks a JWT_SECRET claim citing only USER_CACHE_TTL_SECONDS, but does not establish semantic correctness of arbitrary prose. Full schema, citation, sensitive-pattern, and artifact validation remain in force.
