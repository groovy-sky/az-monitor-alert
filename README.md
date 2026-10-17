# az-monitor-alert

A **placeholder run-to-completion Go demo**, not an instrumented production
workload or an Azure deployment. Replace its simulated work with your real work
boundary. This repository does not provision resources or send email itself:

```text
execution.Run → JSON stdout → Container Apps environment → Log Analytics
             → three scheduled query rules → existing action group → email
```

Success means business work completed, not container startup, HTTP acceptance,
or an alert resolving. There is no HTTP server or exposed port.

## Run locally

Requires Go 1.24+; Docker is optional. From this repository's directory:

```sh
go run ./cmd/alert-example
EXAMPLE_MODE=failure go run ./cmd/alert-example
EXAMPLE_MODE=panic go run ./cmd/alert-example
EXAMPLE_MODE=wait EXECUTION_TIMEOUT=1s go run ./cmd/alert-example
WORKLOAD=example EXECUTION_ID=local-run-001 EXAMPLE_DELAY=100ms go run ./cmd/alert-example

docker build -t az-monitor-alert .
docker run --rm -e EXAMPLE_MODE=success -e WORKLOAD=example az-monitor-alert
docker run --rm -e EXAMPLE_MODE=failure az-monitor-alert
docker run --rm -e EXAMPLE_MODE=panic az-monitor-alert
docker run --rm -e EXAMPLE_MODE=wait -e EXECUTION_TIMEOUT=1s az-monitor-alert
```

Failure, panic, cancellation, and timeout exit nonzero. The wrapper catches
panics from the wrapped work and returns a safe error, without raw error text,
panic values, or stack traces. `go run` also reports a failed command's exit.
The Docker runtime is a `scratch` image, runs `/alert-example` as
`USER 65532:65532`, and contains no shell.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `WORKLOAD` | `example` | Stable business workload name |
| `EXECUTION_ID` | empty | Logical run ID; generated when empty |
| `EXAMPLE_MODE` | `success` | `success`, `failure`, `panic`, or `wait` |
| `EXECUTION_TIMEOUT` | `10m` | Cooperative context deadline |
| `EXAMPLE_DELAY` | `0s` | Simulated delay before the example outcome |

`wait` waits for cancellation/deadline. A context deadline is **not** a forceful
kill: real work must honor `ctx.Done()` and pass the context to blocking APIs.
Cancellation observed before `Run` returns overrides success or an ordinary work
error; a recovered panic remains a panic outcome. Noncooperative work cannot be
stopped by the wrapper. Panics in other goroutines, `os.Exit`, and
`runtime.Goexit` are outside the wrapper's completion contract.
SIGKILL, OOM, process/container loss, or panic in a separate goroutine may prevent
a terminal event. Shutdown handling is best-effort.

## Integrate at the real execution boundary

Package `execution` exposes:

```go
func NewLogger(io.Writer) *Logger
func (logger *Logger) Run(ctx context.Context, workload, executionID string,
    work func(context.Context) error) error
```

For example, in your worker (replace `runBusinessWork` with your implementation):

```go
import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/groovy-sky/az-monitor-alert/execution"
)

var logger = execution.NewLogger(os.Stdout)

func runOnce(parent context.Context, executionID string) error {
    ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
    defer cancel()
    err := logger.Run(ctx, "daily-import", executionID, runBusinessWork)
    if err != nil {
        fmt.Fprintln(os.Stderr, err) // safe wrapper error, not the raw work error
    }
    return err
}
```

Call once per actual work attempt, including each retry; don't wrap only process
startup or queue acknowledgement. Reuse the logical `executionID` across retries
and generate a new ID for each new logical run. Each `Run` generates its own
attempt ID. Don't run retries with the same attempt ID or infer final logical-run
failure from one failed attempt. A retry can produce both failure and success
alerts; final-after-retries semantics need a separate authoritative signal.
Use one shared logger for concurrent attempts sharing an output writer: it
synchronizes each complete JSON-line `Write`. Separate logger instances writing
to the same writer require caller-provided synchronization.

The logger writes events synchronously; if you provide a buffered writer, you own
its flushing and error handling before exit. A failed `started` write aborts
before calling work. A failed terminal write returns a joined safe error; writes
are not retried because partial writes cannot reliably be repaired. Returned
errors use fixed sentinel summaries, never raw work, panic, or writer errors.
Output/validation failures are not business outcomes; investigate missing
telemetry too. The package does **not**
suppress or sanitize logs emitted directly by your work, dependencies, or other
goroutines. Audit those independently.

### Event contract (version 1)

- `schema`: `"az-monitor-alert.execution"`; `schema_version`: integer `1`.
- `event`: `started`, then one `succeeded` or `failed` on normal wrapper completion.
- `timestamp`: UTC RFC3339Nano (`Z` suffix); distinct from Azure's
  `TimeGenerated` log-record timestamp.
- `workload`, `execution_id`: nonempty, at most 128 ASCII characters from
  `A-Z a-z 0-9 . _ : -`. They are correlation labels, **not secret stores**:
  do not include credentials, personal data, business payloads, or sensitive IDs.
- `attempt_id`: new random 16-byte value, encoded as 32 lowercase hex characters.
  An empty caller execution ID is generated with the same format.
- Terminal `duration_ms`: nonnegative int64 elapsed milliseconds for this attempt,
  not a timestamp, total retry duration, or business performance guarantee.
- Only `failed` carries these allowlisted fields:

| `error_code` | Constant `error_summary` |
| --- | --- |
| `work_failed` | `Work failed` |
| `panic` | `Work panicked` |
| `canceled` | `Work canceled` |
| `timeout` | `Work deadline exceeded` |

Illustrative static JSON (timestamps are examples, not current query fixtures):

```json
{"schema":"az-monitor-alert.execution","schema_version":1,"event":"started","timestamp":"2026-10-17T06:00:00Z","workload":"example","execution_id":"00112233445566778899aabbccddeeff","attempt_id":"11112222333344445555666677778888"}
{"schema":"az-monitor-alert.execution","schema_version":1,"event":"succeeded","timestamp":"2026-10-17T06:00:01Z","workload":"example","execution_id":"00112233445566778899aabbccddeeff","attempt_id":"11112222333344445555666677778888","duration_ms":1000}
{"schema":"az-monitor-alert.execution","schema_version":1,"event":"started","timestamp":"2026-10-17T06:01:00Z","workload":"example","execution_id":"aabbccddeeff00112233445566778899","attempt_id":"9999aaaabbbbccccddddeeeeffff0000"}
{"schema":"az-monitor-alert.execution","schema_version":1,"event":"failed","timestamp":"2026-10-17T06:01:02Z","workload":"example","execution_id":"aabbccddeeff00112233445566778899","attempt_id":"9999aaaabbbbccccddddeeeeffff0000","duration_ms":2000,"error_code":"work_failed","error_summary":"Work failed"}
```

## Container Apps hosting choice

Prefer **Azure Container Apps Jobs** for finite, manual, scheduled, or event-driven
run-to-completion work; configure deadlines, retries, parallelism, and idempotency
for your workload. Verify a Job's emitted app/job identity and table columns in
your workspace before adapting the app-scoped query placeholders below.

For a regular Container App, disable ingress: this example serves no HTTP.
Regular apps are long-running services; a process exit/restart can repeat the
example work (even after success). A production regular-app worker needs a
durable queue/lease, a processing loop, logical execution IDs, and idempotent
business operations. Scaling/restarts are not a reliable scheduler.
No deployment or infrastructure is included here.

## Azure portal setup (operator-run, not performed by this repository)

### 1. Connect the environment to the workspace

**New setup: prefer Azure Monitor, not the legacy destination.** Microsoft's
[migration guide][migration] states that the HTTP Data Collector API used by the
legacy destination reached end of support on **September 14, 2026**. Existing
ingestion continues for TLS-compliant clients with only critical security fixes;
that is not a recommendation to build a new setup on it. The three checked-in
`.kql` files intentionally preserve the requested **legacy `_CL` contract**.
Adapt **all three** as described below before creating rules for Azure Monitor.

#### Recommended Azure Monitor destination

1. Open the **Container Apps environment → Monitoring → Logging options**.
   Select **Azure Monitor**, **Save**, wait for completion, and refresh.
2. Open **Monitoring → Diagnostic settings → Add diagnostic setting**.
   Give it a unique name. Select **Container App console logs** and
   **Container App system logs** (or `allLogs` if intended).
3. Under **Destination details**, select **Send to Log Analytics workspace**,
   choose the intended subscription/workspace, and **Save**. Verify the saved
   categories and destination. App-level metrics diagnostic settings are not a
   substitute for environment console-log routing. Avoid duplicate routes.
4. Generate recognizable console events and inspect **workspace → Logs** using
   `ContainerAppConsoleLogs | getschema`. Validate fresh records and their
   environment/app/container identity before enabling alerts.

The verified [resource-specific console schema][console-table] has
`TimeGenerated`, `EnvironmentName`, `ContainerAppName`, `ContainerName`, and `Log`.
In **each** supplied `.kql` file, replace the single source line
`ContainerAppConsoleLogs_CL` with this source/projection, retaining the rest of
that file's query, its indentation/nesting, and exact 1-hour/2-hour bounds:

```kusto
ContainerAppConsoleLogs
| project TimeGenerated,
    EnvironmentName_s = EnvironmentName,
    ContainerAppName_s = ContainerAppName,
    ContainerName_s = ContainerName,
    Log_s = Log
```

This explicitly normalizes the new table to the existing query body's names;
it does **not** union tables, route logs, or change the stored schema. Run and
validate the complete adapted queries, including the missing-result anti-join,
before provisioning the rules below. A Job may require an additional deliberate
scope adaptation after inspecting its actual app/job identity.

Switching destinations stops fresh records arriving in the `_CL` tables;
historical records remain there for their retention period. Do **not** leave
unadapted `_CL` rules monitoring only old data, or silently mix the two schemas.
Follow the [migration guide][migration] for activation delays (up to 90 minutes),
collection-gap planning, canary validation, and alert cutover. Log streaming and
other health signals can help during cutover. This repository performs no
migration. Azure Monitor destination availability also depends on environment
type; the guide currently excludes Express environments.

#### Requested legacy Log Analytics contract (where still available)

1. Open the **Container Apps environment**, not just the app. Select
   **Monitoring → Logging options → Azure Log Analytics**, choose the intended
   existing Log Analytics workspace, and **Save**. Use this only for an explicitly
   accepted existing legacy integration, subject to the support caveat above,
   not as the new-setup default.
2. Run a known example in that environment. Allow several minutes for ingestion.
   Open the **workspace → Logs**; confirm console events, environment, app, and
   container identity before creating alerts. Also retain system logs for crash,
   OOM, and restart investigation; they do not prove business success.
3. The checked-in queries target the **environment Log Analytics destination**
   with this verified custom-table schema:

   | Purpose | Table/column |
   | --- | --- |
   | Console table | `ContainerAppConsoleLogs_CL` |
   | Environment | `EnvironmentName_s` |
   | App | `ContainerAppName_s` |
   | Container name | `ContainerName_s` |
   | Raw JSON line | `Log_s` |
   | Azure log-record time | `TimeGenerated` |

   Microsoft documents the environment/app/log columns in its console table
   listing and `ContainerName_s` in the app console query example:
   [Container Apps log monitoring][logs]. This is **not**
   `ContainerAppEnvironmentName_s`, nor the different resource-specific
   `ContainerAppConsoleLogs` table with unsuffixed columns. Inspect your workspace:

   ```kusto
   ContainerAppConsoleLogs_CL
   | getschema
   ```

   ```kusto
   ContainerAppConsoleLogs_CL
   | where TimeGenerated between (ago(1h) .. now())
   | where EnvironmentName_s == "<ENVIRONMENT_NAME>"
   | project TimeGenerated, EnvironmentName_s, ContainerAppName_s, ContainerName_s, Log_s
   | take 20
   ```

See [logging options][options] for environment settings and destination behavior.

### 2. Verify the existing action group

In **Monitor → Alerts → Action groups**, open the existing group. Confirm the
intended email recipients are enabled, have completed any required email
verification/confirmation (current documentation describes OTP verification),
and haven't unsubscribed. Enable **Common alert schema** for the email
notification. Use **Test** with a log-search alert sample and confirm delivery,
including spam/quarantine checks, with the real recipients. Reuse this group on
all three rules; no application-side SMTP key or alert-management permission
is necessary. See [action groups][actions].

### 3. Create three workspace-scoped scheduled query rules

For Azure Monitor, first apply the source/projection replacement above to **all
three queries**; use the files unchanged only for the accepted legacy route.
Then open each query, replace **all** `<ENVIRONMENT_NAME>`, `<APP_NAME>`,
and `<CONTAINER_NAME>` placeholders, and run the entire query in **workspace →
Logs**. Choose a workspace/scope combination that unambiguously identifies your
environment; names can collide if unrelated environments feed one workspace.
Queries do not depend on saved functions or another query file.

| Rule / query | Meaning | Exact aggregation window and query time range | Suggested severity |
| --- | --- | --- | --- |
| `execution-succeeded` / [succeeded.kql](queries/succeeded.kql) | Observed completed attempt | **1 hour** | Informational (4) |
| `execution-failed` / [failed.kql](queries/failed.kql) | Observed failed attempt | **1 hour** | Error (1) |
| `execution-missing-result` / [missing-result.kql](queries/missing-result.kql) | Overdue start, outcome **unknown** | **2 hours** | Warning (2) |

For **each** rule:

1. In **Monitor → Alerts → Create → Alert rule**, set **Scope** to the exact
   **Log Analytics workspace**, not the Container App, environment, subscription,
   or resource group.
2. Under **Condition**, select **Custom log search**, paste the full query, run
   its preview, and continue editing the alert.
3. Set **Measure = Table rows** (number of query results), with count/total row
   aggregation, **Static threshold → Greater than → 0**. Do not select numeric
   `duration_ms`, or replace the query with `summarize count()` (which would
   return one row even when the count is zero).
4. Set **Aggregation granularity / Window size** to **1 hour** for terminal
   rules and **2 hours** for missing results. Under advanced conditions, set
   **Override query time range** to that same **1 hour / 2 hours**. The query
   bounds and effective alert scan must match; a portal default five-minute
   scan would lose old starts and cause incorrect/missed results.
5. Set **Frequency of evaluation = 5 minutes** and **Number of violations = 1**
   (one violating evaluation triggers; do not require repeated violations).
   Keep the evaluation period aligned with the configured window.
6. **Split by dimensions** on **`workload` only**. Select all intended stable
   workload values, including future values where appropriate. Leave resource
   ID targeting unset so this remains workspace-scoped. **Do not** split by
   execution/attempt IDs, timestamps, durations, error text, or container replica:
   those values are retained for investigation, not alert identity.
7. Under **Actions**, select the existing, tested action group.
8. Under **Details**, choose a rule resource group/region, the name/severity
   above, enable the rule, and select **Automatically resolve alerts**
   (**stateful**). Review, then create. Ensure the rule's query identity has
   workspace read access and the operator has rule-write/action-group-read
   permissions. The application itself only needs to emit stdout/stderr.

See Microsoft's [log-rule creation guide][rules] for current portal labels,
query restrictions, dimensions, permissions, and the maximum **two-day** query
scan. Verify the saved settings and preview with actual ingested events, rather
than relying on inferred defaults.

## Query and notification semantics

- All three queries bound both `TimeGenerated` and parsed UTC event time; filter
  environment/app/container, schema marker/version, types, IDs, and timestamps;
  require terminal integer duration ≥ 0; and deduplicate
  **`workload + execution_id + attempt_id + event`** while retaining those IDs.
  Failure fields must match the constant code/summary pairs. Invalid/malformed
  events are excluded, not relabeled as failures. A malformed terminal may
  consequently leave a valid start overdue; monitor telemetry quality separately.
- Terminal windows overlap (1 hour queried every 5 minutes) intentionally, giving
  late logs several opportunities to be observed. Deduplication collapses copied
  lines **within an evaluation**, not across evaluations or email deliveries.
  Overlapping runs and retries remain separate rows through their attempt IDs.
  Do not reuse a logical ID for unrelated runs.
- The missing-result query scans a **full 2-hour horizon**. It anti-joins starts
  with **either** valid terminal outcome across that entire horizon, keyed by
  workload/execution/attempt. Only starts at least **10-minute maximum runtime +
  5-minute ingestion grace** old qualify. Fresh starts are excluded; a recent
  terminal can suppress an old start, even if the terminal isn't itself overdue.
  Tune the runtime/grace to measured work and ingestion latency, together with
  the application timeout and horizon. The horizon must exceed runtime + grace
  with room for late logs and evaluation. These are example bounds, not an SLA.
- **Missing result is unknown, not confirmed failure.** Crash/OOM/forced exit,
  logging failure, stuck work, and delayed/lost terminal logs can look alike.
  A late terminal can clear the condition. Beyond the 2-hour horizon, old starts
  age out and cannot be detected by this query. Longer runs require a larger
  supported horizon or a durable pending-execution design, not unbounded queries.
- A crash before `started`, a lost start log, or a job never launched is invisible
  here. Compare an **independently stored expected-run source** (scheduler/queue
  ledger with execution IDs, expected times, and deadlines) against observations
  to detect missing starts. That reconciliation is not implemented here.
  Arbitrary unscheduled runs with no independent record cannot be detected.
- A stateful rule tracks **each workload's condition**, not each execution.
  Frequent successes/failures or overlapping overdue runs can keep it fired and
  suppress subsequent notifications. At five-minute frequency Microsoft documents
  resolution after the condition is absent for **three frequency periods**;
  events must first age out or otherwise clear. Resolved notifications may be
  sent via email/webhooks. **Resolution is not success**, repair, or evidence of
  a particular run's result. See [alert state][state].
- Stateless rules can repeatedly notify about the same event during overlapping
  windows. Neither mode promises exactly-once or one-email-per-run delivery.
  Built-in emails are **aggregated operational alerts**, not execution reports.
  They provide alert/query context, not an arbitrary report body or attachment.
  Custom email subjects/properties do not change that guarantee.
- Expect several minutes of ingestion delay **plus** evaluation cadence and
  notification delivery time. Late data outside the windows is missed, and
  late/lost logs can produce false missing-result alerts. Clock skew can exclude
  future/old events; keep worker clocks synchronized. No real-time delivery SLA
  is implied.
- Budget for log ingestion/retention, alert evaluations, and dimension time
  series. Stable workload grouping limits cardinality but does not eliminate
  costs. Action-group notification rate limits, email filtering, alert processing
  suppression, and receiver unsubscribes can affect delivery. Review current
  [action-group limits][actions] and [Azure Monitor service limits][limits].

## Validation

Copyable local checks from the repository directory (race detection requires a
supported platform and C toolchain):

```sh
gofmt -w ./cmd ./execution
go test ./...
go test -race ./...
go vet ./...
```

### Operator-run Azure checks (not executed here)

Use a **disposable, isolated test environment/Job**, its intended workspace,
adapted queries for its logging destination, and a test action-group audience.
Never deliberately kill production work. Disable automatic Job retries for this
test and use one replica/attempt, so a replacement attempt cannot mask the result.

1. Confirm console collection, the three query scopes/windows, workload dimension,
   and action-group test delivery using the portal steps above.
2. In the disposable Job's container configuration, set the following environment
   values and start one execution for each case (save the configuration before
   starting each execution). Use fresh IDs if repeating these tests:

   | `WORKLOAD` | `EXECUTION_ID` | `EXAMPLE_MODE` | `EXECUTION_TIMEOUT` |
   | --- | --- | --- | --- |
   | `alert-validation` | `azure-success-001` | `success` | `10m` |
   | `alert-validation` | `azure-failure-001` | `failure` | `10m` |
   | `alert-validation` | `azure-panic-001` | `panic` | `10m` |

3. After ingestion, run the full success/failure queries in workspace Logs.
   Confirm the success ID appears only in the success query; failure and panic
   IDs appear in the failure query with `work_failed` and `panic`, respectively.
   Inspect the paired start/terminal JSON and matching attempt IDs. Confirm none
   appears in the missing-result query. Verify fired alerts and delivered emails;
   because the failure and panic share a workload, the second may correctly
   produce no new stateful notification while the first keeps the condition fired.
4. **Intentional disruptive missing-result test:** start a new isolated execution
   with `WORKLOAD=alert-validation-missing`, `EXECUTION_ID=azure-missing-001`,
   `EXAMPLE_MODE=wait`, and `EXECUTION_TIMEOUT=10m`. Confirm its `started` event
   in the workspace and record its UTC timestamp and generated attempt ID.
   While it is still younger than 10 minutes, forcibly terminate that exact
   disposable process with **SIGKILL** using an approved platform/debug mechanism.
   Do not use a graceful stop: it may emit `failed`/`canceled`, which correctly
   suppresses missing-result detection. The scratch image has no shell, and a
   portal Stop operation must not be assumed to guarantee SIGKILL. If your
   environment cannot guarantee force termination, record this test as blocked.
   If the start arrives too late to kill before its deadline, repeat with a fresh
   execution ID rather than treating a timeout terminal as a missing result.
5. Confirm no terminal event exists for that exact execution/attempt. Before
   15 minutes from its event timestamp, the start must not appear as overdue.
   After **more than 15 minutes**, plus ingestion/evaluation time, run the full
   missing-result query: expect `result=unknown_missing_result` with its original
   workload, execution ID, and attempt ID. Confirm the missing-result alert/email
   fires; the failure query must not invent a failed outcome for this attempt.
6. Without additional starts for that workload, observe the row age out after
   the **2-hour start horizon**, followed by the stateful rule's documented
   resolution delay (three five-minute frequency periods). Confirm the resolved
   notification; this is window expiry, **not** evidence that work succeeded.
   Terminal rule rows similarly age out after their 1-hour windows. Preserve
   investigation evidence before cleaning up disposable resources.

Before production adoption, an operator should also exercise
cooperative timeout/shutdown, overlapping attempts/retries, duplicate and late
logs, a start with no terminal, and a missing start using the external ledger.
Inspect query rows/IDs, verify fresh starts do not alert, validate the full-horizon
anti-join, test firing and eventual resolution emails, and confirm suppression
behavior across repeated runs. No Azure validation or deployment is claimed here.

Local validation passed: Go tests, race detection, vet, Docker image build, and
container success/failure/panic/cooperative-timeout scenarios. Azure KQL and action
delivery have not been executed. The forced-SIGKILL Docker check was blocked by
the execution environment; missing-terminal behavior is not claimed as manually
verified.

## Official references checked

Checked against Microsoft's published documentation on 2026-10-17 (including its
official documentation sources where the Learn host was unavailable):

- [Container Apps console/system schema and sample queries][logs]
- [Environment logging destinations, portal configuration, and ingestion delay][options]
- [Legacy destination migration, table/column changes, and alert cutover][migration]
- [Resource-specific console table: verified projection column names][console-table]
- [Create scheduled log-search rules: scope, rows, dimensions, time ranges, state][rules]
- [Azure Monitor alert firing/resolution state][state]
- [Action groups: existing groups, verified recipients, common schema, tests, limits][actions]

The service-limits link above is provided for operators to check current quotas.

[logs]: https://learn.microsoft.com/en-us/azure/container-apps/log-monitoring
[options]: https://learn.microsoft.com/en-us/azure/container-apps/log-options
[migration]: https://learn.microsoft.com/en-us/azure/container-apps/migrate-logs-azure-monitor
[console-table]: https://learn.microsoft.com/en-us/azure/azure-monitor/reference/tables/containerappconsolelogs
[rules]: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-create-log-alert-rule
[state]: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-overview#alerts-and-state
[actions]: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/action-groups
[limits]: https://learn.microsoft.com/en-us/azure/azure-monitor/service-limits