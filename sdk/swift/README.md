# Scruff (Swift SDK)

A thin Swift client over the [`scruff`](https://github.com/hausfold/scruff) binary — the
worktree-lifecycle substrate for parallel coding agents. Shells out to
`scruff` (`Process` + `--json`, `watch --json` for a live NDJSON stream)
rather than talking to a daemon.

macOS only, no iOS/tvOS/watchOS: `Foundation.Process` can't spawn a
subprocess there. Linux works too — `Process` is part of
`swift-corelibs-foundation`. Needs Swift 5.9+ (uses
`FileHandle.bytes.lines` and `AsyncThrowingStream`).

## Install

This README lives at `sdk/swift` in
[`hausfold/scruff`](https://github.com/hausfold/scruff) and is mirrored to
[`hausfold/scruff-swift`](https://github.com/hausfold/scruff-swift) by
[`sync-mirror.sh`](sync-mirror.sh), because SwiftPM requires
`Package.swift` to sit at a repo's root. Send changes to `sdk/swift` in
`hausfold/scruff` — never to the mirror, which is overwritten wholesale on
the next sync.

```swift
.package(url: "https://github.com/hausfold/scruff-swift", from: "1.0.0")
```

To work on the SDK itself, reference it as a local package against a
`hausfold/scruff` checkout instead: `.package(path: "../scruff/sdk/swift")`.

`scruff` itself must be on `PATH`, or pass `ScruffClientOptions(bin: "/path/to/scruff")`.

## Two shapes of usage

**Programmatic (a web backend, an orchestrator).** Every `ScruffClient`
method except the two ending in `Interactive` captures the child's stdout
and returns — safe to call from a server with many concurrent sessions.
`ScruffClient` is a `Sendable` value type; construct one per call if you like.

```swift
import Scruff

let scruff = ScruffClient()

let envelope = try await scruff.list()
for lane in envelope.lanes {
    // occupied/dirty are `Bool?` — nil means "not determined", never
    // coerce it to false.
    print(lane.name, lane.state, lane.occupied as Any)
}

// Create a lane WITHOUT attaching an agent to it — the primitive an
// orchestrator wants. `child`/`spawn` only ever print the new path.
let dir = try await scruff.child("/path/to/some-repo", name: "task-42")
// ...now launch YOUR OWN agent process against `dir`.
```

```swift
// Live updates instead of polling — created/parked/resumed/reaped/changed.
for try await line in scruff.watch() {
    if case .event(let event) = line, event.kind == .created {
        notifyUI(event.lane)
    }
}

// Or scoped to the one lane this session holds — no hello/ready framing,
// and nothing about anybody else's lanes. `.sync` still arrives: it's how
// you learn about a lane that went live before you attached.
for try await event in scruff.watchLane(path: dir) {
    if event.kind == .reaped { endSession() }
}
```

**Interactive (a real terminal app).** `newInteractive` /
`resumeInteractive` inherit the calling process's stdio, so when scruff
execs the configured agent client (`claude`, `codex`, `opencode`, `pi`), it
takes over the real terminal — same as running `scruff new` by hand — and
control returns to you when that session ends.

```swift
// Run in an actual terminal:
try await scruff.newInteractive("task-42")
// ... the agent owned the screen; you're back here when it exits.
```

**Do not call `newInteractive` from a server.** It execs the agent client
unconditionally, without checking for a TTY, so piped stdio blocks
forever. Use `resume()` from a server instead — it detects piped stdout
and prints the reopen command as text rather than exec'ing.

## Holding a session open: leases

`scruff`'s sweep (`reap`) needs to know a checkout is in use — `lsof`
answers that on a human's machine, but a server holding one session per
lane has no pane or shell cwd'd anywhere, so it says so itself with a lease:

```swift
let lease = try await scruff.lease(path: laneDir) // refreshes on an interval, < the 90s TTL
// ... serve the session ...
await lease.release()
```

Pass `pid:` instead when the lease should track a real local process —
the OS then drops it the instant that pid dies, no refresh loop needed.

A lease can only **save** a lane from `reap`, never condemn one —
absence of a lease isn't proof nobody's there.

`scruff.lease(...)` is a throwing `async` factory (an `actor Lease` comes
back once the first heartbeat succeeds), so a failure to take the lease
raises immediately instead of surfacing on the next refresh or release call.

## `watch()` cleanup

`watch()`/`watchLane(path:)` return an `AsyncThrowingStream`. The
underlying process is killed via the stream's `onTermination` handler,
which fires when you `break` out of a `for try await` loop or the
enclosing `Task` is cancelled — the only ways to stop it, since `watch`
has no built-in end condition.

## Types for a UI layer

`Types.swift` has no dependencies beyond `Foundation`'s `Codable`
machinery — `Process`/`Pipe`/`FileHandle` only appear in `Exec.swift`/
`Watch.swift`/`Client.swift`. Import just the types if something else in
your app needs to model the same wire shape (e.g. a SwiftUI view model
fed by your own actor wrapping `watch()`):

```swift
import Scruff // ScruffLane, WatchEvent, …
```

`LaneState`, `LandedVerdict`, `LandedVia`, and `WatchEventKind` are not
Swift `enum`s — they're `ScruffOpenEnum<Tag>`, a `RawRepresentable` string
wrapper with `static let` cases. A real `enum: String, Codable` throws a
`DecodingError` on an unrecognized value; an unknown value here decodes
as opaque instead of failing. Compare with `==` and the `static let`
constants (`lane.state == .live`) — exhaustive `switch` isn't available.

## What's not here yet

- `hook create`/`hook remove` have no wrapper — shell out via the free
  `run()` function if you need them.
- `ScruffLane` doesn't include the `--json` envelope's future fields
  (`pr`, `overlap`, `ahead`/`behind`) — they aren't on the wire yet.

## Testing

`Tests/ScruffTests/fake-scruff.sh` stands in for the real binary so tests
don't need a Go build.

```
swift test
swift build
```
