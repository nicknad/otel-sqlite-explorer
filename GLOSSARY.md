# otel-sqlite-explorer

A read-only web UI and JSON API for querying OpenTelemetry logs stored in SQLite.
The terms below are explorer-local; the sink repo (github.com/nicknad/otel-sqlite)
keeps its own canonical GLOSSARY.md, and cross-repo sharing stays open decision D-4
in docs/COUPLING-AUDIT.md — neither decided nor blocked by this file.

## Language

**Query pipeline**: the module that turns a log request — a form's filters or a DSL
query — into a Page: validation, search-shape selection, compilation, execution,
pagination. Everything between a request entering and rows being ready.
_Avoid_: query service, request handler, controller.

**Page**: one result of the Query pipeline — the rows of the requested window plus
the facts pagination needs (whether a next or previous window exists, the offsets to
request next) and the canonical echo of the filters actually applied.
_Avoid_: result set, response, DTO.

**Compile pipeline**: the part inside the Query pipeline that turns a validated,
normalized query into one parameterized SQL statement. Sharpens the older, narrower
use of "query pipeline" (meaning only the DSL and compiler) in docs/API-DB-QUERY.md.
_Avoid_: reusing "query pipeline" for this narrower meaning.

**Search capability**: the fact the db seam exposes about the search index — its
sink-defined mode (`off`, `body`, `full`, or a foreign database) and its rebuild
status — from which the pipeline decides between index search and substring
fallback. The explorer detects it; it never builds, repairs, or assumes the index
shape.
_Avoid_: FTS flag, feature toggle.
