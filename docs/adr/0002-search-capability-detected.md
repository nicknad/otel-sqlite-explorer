# 0002 — Search capability is detected, not assumed (closes D-2)

A logs_fts table alone proves nothing: the sink leaves one behind in every
search mode, including off, where nothing maintains it — so table-existence
detection reported full-text search as available and silently returned zero
rows for queries that had matches (R1, live-reproduced against an off-mode
database). db now classifies search capability once at Open: an fts5 virtual
table kept in sync by log_event triggers is active; a table without
maintenance is unmaintained, and MatchExpr nodes fall back to substring
matching. This closes docs/CONTRACT.md's open decision D-2 with the
trigger-probe option (a) as the primary mechanism; the state-row option (b)
remains a compatible extension for mode-aware copy on 006+ databases, and
row-count probing stays rejected (wrong for rebuilds and empty databases).
