**Decision**: `partial` means "stream cut after headers were sent (a preempted stream)", and `failed` covers "turned away or errored". The mapping is stated once and no rule is given for the cases in between.
**Problem**: The boundary is under-specified. Is a client disconnect mid-stream `partial` or `failed`? Is an upstream error before the first byte `failed`, and one after it `partial`? Is a non-streaming request that times out after the upstream began work `failed`? The embed lane and the batch lane may not stream at all. Each handler will decide for itself, and the outcome field will drift. Dashboards and alerts built on it will then be unreliable.
**Strongest alternative**: Write a decision table (stage reached × cause → outcome, `err_type`) and test every row. Define `partial` by a mechanical fact, "response headers were written and the handler did not finish cleanly", not by cause.
**Reversibility**: painful (once alerts and queries key on the values)

**Decision**: `request.served` carries `outcome=ok`, and `request.rejected` carries `outcome=failed`. This is also the only place the plan names an event for `partial`.
**Problem**: Two of the three outcomes have named events. `partial` has none. Also, `request.served` keeps the word "served" in the event name while R11 removes it from the outcome. That is confusing, and the plan calls the event name a stable contract. `rejected` is also wrong for errored or partial requests, because the plan itself says `failed` covers "errored". So the event name and the outcome disagree about what happened.
**Strongest alternative**: Use one request-completion event, such as `request.completed`, and let `outcome` and `err_type` carry the distinction. If separate events are kept, name one for each outcome. Settle these names now, because they are the contract.
**Reversibility**: one-way door (event names are a stable contract that queries will depend on)

**Decision**: A single process-wide `run_id` generated at start, and "a different `run_id` on a second start" is tested.
**Problem**: Under 16 hex characters from `crypto/rand` it is fine for uniqueness. The real gap is the meaning of "run". Does it cover a config reload, an in-process restart of the logger, or a supervised child? The test for "second start" implies the logger is built in a way that can be started twice in one test process. That tends to push toward either a package-level global (hard to test) or a constructor. The plan does not say which. The `crypto/rand` error path is also unaddressed: what happens if it fails at startup, and does the Broker crash or fall back to a weak id?
**Strongest alternative**: Build the logger as a constructor that takes the `run_id` (injectable in tests) and fail fast on a rand error. State what "run" means in one sentence.
**Reversibility**: easy

**Decision**: "Make `event` a required argument... a missing event is a compile error", and "the implementer lists [existing non-request lines] by searching for every log call".
**Problem**: This is a signature change across every call site, done by search. Nothing proves the list is complete except the compiler, which is good, but event names for the non-request lines are invented ad hoc by the implementer ("such as `yield.started`"). That makes them a one-way contract that nobody reviews. A required string argument still allows `""` or a name with no dot. The plan only checks that the constants contain a dot, not that every call uses a constant.
**Strongest alternative**: Use a named type, `type Event string`, with the constants as the only values. Add a lint or test that rejects untyped string literals at call sites. Add the full event list to the plan or the review, so the names get reviewed once.
**Reversibility**: painful

**Decision**: Request-lane handlers are changed by hand in "interactive, batch and embed lanes", each building its own `outcome` and `err_type`.
**Problem**: Three lanes plus yield and jobs mean at least five places that decide the outcome. This is where drift happens. The plan says "no new abstractions", but the likely result is repeated mapping code. The implicit decision is that outcome mapping lives at call sites and not in shared middleware.
**Strongest alternative**: Use one request-completion helper or middleware that takes (status, bytes written, error class) and returns the outcome and `err_type`. It does not need to be a public abstraction. It would be a single private function with callers in all three lanes, which satisfies the "real second caller" rule. Then name it in `## New abstractions`.
**Reversibility**: painful

**Decision**: `err_msg` is "a short fixed text per `err_type`".
**Problem**: A fixed text per type loses the information that makes `err_msg` useful, so the field adds almost nothing over `err_type`. An operator debugging an `upstream` failure gets no status code, no upstream host and no timeout value. The plan also does not say where the real error goes. If it is dropped, debugging gets worse than today. If an operator can't tell a connection refused from a 502, the change is a regression in diagnosability.
**Strongest alternative**: Keep the fixed `err_msg` and add a separate sanitized detail, such as an upstream status code, an error class enum, or a redacted error string passed through an allow-list. The leak test then covers it.
**Reversibility**: easy (adding fields later is cheap, but missing history can't be recovered)

**Decision**: The query string is dropped from the path: "log the path without its query string".
**Problem**: This appears only under Risk areas and is not in the API contract table or the Design decisions. It is a real behavior change that the leak test needs to cover. Path segments can also carry secrets or identifiers (for example `/jobs/{id}` or token-in-path schemes). Only the query is handled. High-cardinality paths are also not mentioned, and that matters for any Loki query on path.
**Strongest alternative**: Put it in the contract as a rule, log the route template (for example `/jobs/{id}`) when the router has one, and test with a token in both the query and the path.
**Reversibility**: easy

**Decision**: "Leave HTTP response headers and stream trailers unchanged... Those still say `served`."
**Problem**: This creates two vocabularies for the same fact in one system. A log line says `ok` and the trailer says `served`. Anyone correlating a client-visible trailer with a log line has to know the mapping. The plan treats that as out of scope, but it never says this is a deliberate known inconsistency to be followed up, and a one-way door appears if clients start depending on the trailer text.
**Strongest alternative**: Keep the scope as is, but record it as a named follow-up with an owner and state the mapping (`served` → `ok`) in the doc. If R11 is meant system-wide, the plan should say why it is not.
**Reversibility**: painful (clients may parse the trailer)

**Decision**: A repo-scan test over `deploy/` fails if a Loki label config lists `run_id` or `req_id`.
**Problem**: This tests a static file, not what the running system ships. Labels can be set by a promtail or Alloy pipeline, by Docker labels, or by a config outside `deploy/`. "Lists" is also vague: a text grep can false-positive in comments and false-negative on a templated or regex label rule such as one that promotes all JSON keys. The plan does not say what the config format is, because it has not read it.
**Strongest alternative**: Parse the actual config and assert on the label set. Or cut it, since the plan leaves the label config alone and `run_id` and `req_id` are new. If it stays, name the file and its format.
**Reversibility**: easy

**Decision**: The metrics golden file, captured from pre-change code, with a comparison test.
**Problem**: Nothing in the requirements shown makes metrics part of this change, and the plan itself says "no metrics changes". The test freezes the metrics surface, which makes legitimate later metric changes fail a test about logging. The golden file also depends on a "fixed request sequence" with timing-based series (wait time, histograms) that tend to be flaky. The capture-first rule is also procedural and easy to get wrong.
**Strongest alternative**: Assert only that the set of metric names and label keys is unchanged, not values, and only if the logger could plausibly touch them. Otherwise drop it and rely on the diff.
**Reversibility**: easy

**Decision**: The test shells out to `logging-check.py` at a path read from an environment variable.
**Problem**: The Go test suite now depends on an external Python script outside the repo, and on its version. CI, other contributors and any unattended job need the variable set. When the checker changes its rules, the Broker's tests fail without a Broker change. "Fail loudly if missing" is right, but it makes the Go tests unrunnable by default. It is also a hidden coupling to Preston's global tooling.
**Strongest alternative**: Vendor a pinned copy of the checker (or its rule file) into `testdata/` and record the source version. Or gate it behind a build tag and add a separate CI target, while the Go-level assertions listed in the plan stay in the default suite.
**Reversibility**: easy

**Decision**: `service` "value unchanged", `schema_version` "as today", and the new fields (`event`, `run_id`, `outcome`, `err_type`) are added without a version bump.
**Problem**: The plan changes the meaning of existing data (`served` removed, the `level` set enforced, new required fields) but leaves `schema_version` as it is. Any consumer that pins to a schema version will silently receive a different shape. The plan also never says whether a log line with `level` outside the set is dropped, coerced or panics when "enforced".
**Strongest alternative**: Decide explicitly whether `schema_version` bumps. If R-numbered requirements forbid it, say so. State the behavior for an invalid level (coerce to `error` and log a self-warning is the safest).
**Reversibility**: one-way door (once downstream parsers key on the schema)

**Decision**: Logging goes to stdout, with lines built by one logger, and "any line that bypasses the logger would miss `run_id`".
**Problem**: The plan names this as a risk but proposes no mechanism to detect it. Panics, the Go `log` package used by the standard library or dependencies, and `http.Server.ErrorLog` write unstructured lines to stderr or stdout. These lines will fail the checker in production but not in the handler tests. Concurrent writes to stdout are also not addressed: interleaved lines would produce invalid JSON.
**Strongest alternative**: Route `log.SetOutput` and `http.Server.ErrorLog` through the logger, and serialize writes with one mutex or a single `Write` per line. Add a test that greps the repo for direct `fmt.Print*` and `log.Print*` calls.
**Reversibility**: easy

**Decision**: Level set includes `critical` and `debug`, but the plan names events only at `info` and `warn`.
**Problem**: `critical` and `error` have no stated mapping. Nothing says which failures are `error` and which are `critical`, and the plan never says whether `request.rejected` for `queue_full` is `warn` while an upstream failure is `error`. Level choice per event is implicitly left to the implementer. Level drives alerts, so it is a hidden contract.
**Strongest alternative**: Add the level to the event table, one row per event, along with its outcome and `err_type`.
**Reversibility**: painful (alert rules depend on it)

**Decision**: The plan spends a full section on seams but `## New abstractions` says "None".
**Problem**: The plan introduces a set of event-name constants in one file, an `Event`-like contract, an `err_type` vocabulary and a fixed-text table. These are shared schemas with multiple callers (every handler, yield, jobs, tests) and should be listed. This is a rule violation in the plan as written. Listing "None" hides the one thing reviewers most need to check: the event and `err_type` registry.
**Strongest alternative**: List the event/`err_type` registry file under New abstractions, naming its callers (handlers, yield, jobs, the dotted-name test). That is a legitimate second caller, so it passes the bar. Alternatively, state that the registry is just constants in the logger package and not a new module.
**Reversibility**: easy
