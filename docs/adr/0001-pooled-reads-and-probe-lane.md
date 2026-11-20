# 0001 — Pooled read connections with a dedicated probe lane

The explorer's db layer serialized every query behind one connection and one
mutex acquired before the deadline started, so queued requests and /healthz
had no budget of their own (model-checked rules R4/R5, live-reproduced in
docs/API-DB-QUERY.md). We deleted the mutex, sized a small read pool
(maxConcurrentReads = 4) whose admission honors each query's context, and gave
Ping a dedicated one-connection probe lane that never queues behind queries;
the second connection is the deliberate cost of honest liveness.

Considered: keeping one connection behind a semaphore (rejected — the pool is
the admission gate database/sql already provides, and deleting a mechanism
beats repairing it); a larger pool (rejected — the database file is written
live by the sink, and one small deliberate reader footprint is policy;
revisit only with measured queueing). If modernc ever surfaces table-lock
errors under this concurrency, drop cache=shared from the DSN first.
