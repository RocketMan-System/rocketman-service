# Graph Report - ProxyBridgeService  (2026-10-28)

## Corpus Check
- 28 files · ~12,118 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 247 nodes · 507 edges · 24 communities (16 shown, 4 thin omitted)
- Extraction: 99% EXTRACTED · 1% INFERRED · 0% AMBIGUOUS · INFERRED: 4 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `4dfc75e4`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- windows/main.go
- TunnelManager
- TunnelManager
- time.Duration
- install-linux.sh
- linux/install.sh
- macos/install.sh
- Supervisor
- linux/uninstall.sh
- macos/uninstall.sh
- AppMonitor
- validate
- tunnel.go
- build-linux.sh
- build-windows.sh
- github.com/xanderwp/proxybridgeservice
- os.Process
- proc_windows.go
- testSupervisor
- PidFile

## God Nodes (most connected - your core abstractions)
1. `Supervisor` - 19 edges
2. `TunnelManager` - 15 edges
3. `AppMonitor` - 14 edges
4. `main()` - 14 edges
5. `TunnelManager` - 13 edges
6. `TunnelManager` - 13 edges
7. `RecentLogStore` - 12 edges
8. `QuarantineMonitor` - 9 edges
9. `main()` - 9 edges
10. `NewHTTPHandlerWithLogs()` - 9 edges

## Surprising Connections (you probably didn't know these)
- `testSupervisor()` --calls--> `NewSupervisor()`  [INFERRED]
  src/shared/supervisor_test.go → src/shared/supervisor.go
- `TunnelManager` --references--> `SingboxLogWriter`  [EXTRACTED]
  src/linux/main.go → src/shared/logs.go
- `TunnelManager` --references--> `PidFile`  [EXTRACTED]
  src/linux/main.go → src/shared/pidfile.go
- `TunnelManager` --references--> `Supervisor`  [EXTRACTED]
  src/linux/main.go → src/shared/supervisor.go
- `NewTunnelManager()` --calls--> `NewSupervisor()`  [EXTRACTED]
  src/linux/main.go → src/shared/supervisor.go

## Import Cycles
- None detected.

## Communities (24 total, 4 thin omitted)

### Community 0 - "windows/main.go"
Cohesion: 0.13
Nodes (24): golang.org/x/sys/windows/svc.ChangeRequest, golang.org/x/sys/windows/svc.Cmd, golang.org/x/sys/windows/svc.State, golang.org/x/sys/windows/svc.Status, net/http.Handler, net/http.Server, NewServer(), ServeWithRetry() (+16 more)

### Community 1 - "TunnelManager"
Cohesion: 0.14
Nodes (13): os/exec.Cmd, sync.Mutex, PlatformHTTPHandler, TunnelManager, addInterfaceToTrustedZone(), ensureInterfaceInTrustedZone(), getTunInterfaceNameFromConfig(), main() (+5 more)

### Community 2 - "TunnelManager"
Cohesion: 0.18
Nodes (7): PlatformHTTPHandler, QuarantineMonitor, TunnelManager, main(), NewQuarantineMonitor(), NewTunnelManager(), resolveBasePath()

### Community 3 - "time.Duration"
Cohesion: 0.18
Nodes (15): net/http.Request, net/http.ResponseWriter, time.Duration, HTTPHandler, ITunnelOperations, LogEntry, LogMirrorWriter, RecentLogStore (+7 more)

### Community 4 - "install-linux.sh"
Cohesion: 0.40
Nodes (12): build_binary(), check_root(), create_unit_file(), detect_goarch(), install_binary(), log_error(), log_info(), log_warn() (+4 more)

### Community 5 - "linux/install.sh"
Cohesion: 0.42
Nodes (11): check_root(), create_unit_file(), detect_arch(), download_binary(), log_error(), log_info(), log_warn(), main() (+3 more)

### Community 6 - "macos/install.sh"
Cohesion: 0.42
Nodes (11): check_root(), create_plist(), detect_arch(), download_binary(), log_error(), log_info(), log_warn(), main() (+3 more)

### Community 7 - "Supervisor"
Cohesion: 0.27
Nodes (5): childProcess, SupervisorHooks, supervisorSession, Supervisor, NewSupervisor()

### Community 8 - "linux/uninstall.sh"
Cohesion: 0.47
Nodes (9): check_root(), log_error(), log_info(), log_warn(), main(), remove_files(), uninstall.sh script, stop_service() (+1 more)

### Community 9 - "macos/uninstall.sh"
Cohesion: 0.47
Nodes (9): check_root(), log_error(), log_info(), log_warn(), main(), remove_files(), uninstall.sh script, stop_service() (+1 more)

### Community 10 - "AppMonitor"
Cohesion: 0.21
Nodes (8): net/http.Client, sync.Once, sync.WaitGroup, time.Time, ITunnelManager, ComputeRestartDelay(), AppMonitor, NewAppMonitor()

### Community 11 - "validate"
Cohesion: 0.70
Nodes (4): validate script, render_progress(), run_build(), show_help()

### Community 12 - "tunnel.go"
Cohesion: 0.50
Nodes (3): TunnelStartResult, TunnelStatus, TunnelStopResult

### Community 18 - "os.Process"
Cohesion: 0.25
Nodes (4): os.Process, ForceKill(), SignalTerm(), sendCtrlBreak()

### Community 19 - "proc_windows.go"
Cohesion: 0.29
Nodes (3): golang.org/x/sys/windows.Handle, KillOnCloseJob, NewKillOnCloseJob()

### Community 20 - "testSupervisor"
Cohesion: 0.57
Nodes (7): testing.T, TestPidFileKillStale(), testSupervisor(), TestSupervisorDetectsExitAndRestarts(), TestSupervisorExitedImmediately(), TestSupervisorStartDuringStop(), TestSupervisorStartStop()

## Knowledge Gaps
- **6 isolated node(s):** `github.com/xanderwp/proxybridgeservice`, `TunnelStartResult`, `TunnelStopResult`, `TunnelStatus`, `build-linux.sh script` (+1 more)
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 42 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **4 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `TunnelManager` connect `windows/main.go` to `TunnelManager`, `proc_windows.go`, `PidFile`, `Supervisor`?**
  _High betweenness centrality (0.099) - this node is a cross-community bridge._
- **Why does `Supervisor` connect `Supervisor` to `windows/main.go`, `TunnelManager`, `TunnelManager`, `testSupervisor`?**
  _High betweenness centrality (0.096) - this node is a cross-community bridge._
- **Why does `TunnelManager` connect `TunnelManager` to `os.Process`, `time.Duration`, `PidFile`, `Supervisor`?**
  _High betweenness centrality (0.059) - this node is a cross-community bridge._
- **What connects `github.com/xanderwp/proxybridgeservice`, `TunnelStartResult`, `TunnelStopResult` to the rest of the system?**
  _6 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `windows/main.go` be split into smaller, more focused modules?**
  _Cohesion score 0.1253968253968254 - nodes in this community are weakly interconnected._
- **Should `TunnelManager` be split into smaller, more focused modules?**
  _Cohesion score 0.1383399209486166 - nodes in this community are weakly interconnected._