# E2E auxiliary files (cbl-api-gateway)

Purpose: support runs per `test/e2e/E2E.md` and analysis of their artifacts.
There are no PowerShell run scripts here (convention `CBL-002` section 7).

| File/directory | Purpose |
| :-- | :-- |
| `stub/` | Test upstream (HTTP): responds 200 to any path and echoes the path, the request id and the authorization flag; the `stub` image is built by the compose stand |
| `summary.template.md` | The `summary.md` run report template |

Rules: utilities must build and run with standard `go`/`docker` commands on any platform;
new auxiliary files are added here and recorded in this table.
