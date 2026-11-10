# Live touch through the WebDriverAgent helper: what it would cost

Date: 2026-11-11. Question: on an iPhone below iOS 27, Pinky's mirror waits for
the finger to lift before the phone moves, because one WDA request plays one
whole gesture (`XCSynthesizedEventRecord`). Could a patched WDA — or a way
around WDA — deliver touch events one by one, in real time? This note reconnoitres;
nothing in the code was changed.

## 1. The input routes WDA has, and how each is done

**Conclusion: every touch route ends in the same place — one complete
`XCSynthesizedEventRecord` handed to `[XCUIDevice.sharedDevice eventSynthesizer]
synthesizeEvent:completion:`. None of them is event-by-event.**

File and line numbers are appium/WebDriverAgent `master` as of 2026-11-11.

| Route | Handler | How it injects |
|---|---|---|
| `POST /actions` (W3C) | `FBTouchActionCommands.m:20-38` | One `XCSynthesizedEventRecord` from `FBW3CActionsSynthesizer`, then `fb_synthesizeEvent:` |
| `POST /wda/tap`, `/wda/doubleTap`, `/wda/twoFingerTap`, `/wda/tapWithNumberOfTaps`, `/wda/touchAndHold`, `/wda/forceTouch`, `/wda/swipe`, `/wda/pinch`, `/wda/scroll`, `/wda/dragfromtoforduration`, `/wda/pressAndDragWithVelocity` | `FBElementCommands.m:81-122` | Same: an `XCPointerEventPath` (press, moves, lift at offsets) inside one `XCSynthesizedEventRecord`; the drag goes through `fb_pressAtCoordinate:forDuration:thenDragToCoordinate:...` (`XCUIApplication+FBTouchAction.m:121-161`) |
| `POST /element/:uuid/click`, `/element/:uuid/value`, `/clear` | `FBElementCommands.m:67-69` | Element taps resolve to a coordinate and the same record path |
| `POST /wda/keys`, typing into fields | `FBElementCommands.m:123, 554-563` | XCTest's keyboard path (`FBTypeText`), not touch |
| `POST /wda/pressButton` | `FBCustomCommands.m:69` | `XCUIDevice.pressButton` — hardware buttons |
| `POST /wda/performIoHidEvent` | `FBCustomCommands.m:75, 396-410` | `XCDeviceEvent deviceEventWithPage:usage:duration:` → `_XCT_performDeviceEvent:`. A raw HID *usage page + usage + duration* — enough for the power button (WDA uses it to lock on iOS 27+: `XCUIDevice+FBHelpers.m:147-161, 359-366`). It has no digitizer contact, no x/y, no move stream: not a touch channel |

So the only injection primitives the WDA process has are two XPC calls into
testmanagerd, `_XCT_synthesizeEvent:` (a whole record) and
`_XCT_performDeviceEvent:` (a hardware-button-like event), from
`XCTMessagingRole_EventSynthesis-Protocol.h` in WDA's own
`PrivateHeaders/XCTest/`. There is no third one.

## 2. What `POST /actions` does server-side

**Conclusion: yes — the request body becomes exactly one
`XCSynthesizedEventRecord`, and the route blocks until that whole gesture has
been played on the phone.**

The path, with the key lines:

1. `FBTouchActionCommands.m:20-38` — `handlePerformW3CTouchActions:` calls
   `fb_performW3CActions:elementCache:error:` on the active application.
2. `XCUIApplication+FBTouchAction.m:85-102` —
   `fb_performActionsWithSynthesizerType:actions:elementCache:error:` builds
   `XCSynthesizedEventRecord *eventRecord = [synthesizer synthesizeWithError:error]`
   and returns `[self fb_synthesizeEvent:eventRecord error:error]`.
3. `FBW3CActionsSynthesizer.m:950-1008` — `eventRecordForActions:` makes
   `[[XCSynthesizedEventRecord alloc] initWithName:@"W3C Touch Action" ...]`
   and `synthesizeWithError:` turns each action into `XCPointerEventPath`s
   (press-down, moves with offsets, `liftUpAtOffset:`) added with
   `[eventRecord addPointerEventPath:eventPath]`. All fingers, all moves, all
   lifts of the request go into this one record.
4. `XCUIApplication+FBTouchAction.m:192-195` —
   `fb_synthesizeEvent:` is `[FBXCTestDaemonsProxy synthesizeEventWithRecord:error:]`.
5. `FBXCTestDaemonsProxy.m:106-133` — the record is handed to
   `[XCUIDevice.sharedDevice eventSynthesizer] synthesizeEvent:record completion:`
   inside `[FBRunLoopSpinner spinUntilCompletion:^{...}]`: the HTTP request
   does not return until testmanagerd calls the completion back.
   Testmanagerd performs the whole record before it replies (the message it
   serves is `_XCT_synthesizeEvent:completion:`,
   `XCTMessagingRole_EventSynthesis-Protocol.h`).

Pinky sees the same thing from outside: `gesturePath` posts one `/actions`
request per gesture (`internal/driver/ios/wda.go:442-473`) and the mirror
records the finger's whole path and plays it back at TouchUp
(`internal/driver/ios/replaytouch.go:107-130`); the measured touch-to-picture
lag on the WDA path is in `docs/notes/mirror-smoothness.md` (297 ms median on
the Simulator, 119-348 ms on the iPhone 13 mini over Wi-Fi).

## 3. Can a patched WDA deliver touch in real time?

**Conclusion: not clear it can — and the honest verdict is "probably not through
XCTest". The one thing that would make it possible is undocumented, and cheap
to test once on a phone.**

- **`pointerDown` in one request, `pointerMove` in the next, finger down
  throughout.** XCTest's XPC protocol has no begin/continue/end: only
  `_XCT_synthesizeEvent:` taking a self-contained record. Appium's own
  documentation of the API says each `XCPointerEventPath` performs a single
  action, its offsets must increase, and it is executed once — events play
  back as one atomically-timed sequence, and a path cannot be extended after
  execution ([input-events guide](https://github.com/appium/appium-xcuitest-driver/blob/master/docs/guides/input-events.md)).
  Nothing public shows a finger staying down across two records.
  Counter-hint: `XCSynthesizedEventRecord` has undocumented
  `beginsPersistentState` / `endsPersistentState` properties
  (`PrivateHeaders/XCTest/XCSynthesizedEventRecord.h`), so records can in
  principle mark state that persists past the record; what that state covers
  is not documented, and I found no public use for touch. **Verdict:
  不清楚 — one small experiment on a phone (two records, the first ending
  without `liftUpAtOffset:`) would settle it.**
- **A continuously driven API.** `XCTRunnerDaemonSession synthesizeEvent:` and
  `FBSynthesizeEventWithRecord` are the same record API WDA already uses
  (`fb_synthesizeEvent:` above). `IOHIDEvent`-level injection needs
  `IOHIDEventSystemClient` and the entitlements `dtuhidd`/backboardd trust;
  the WDA test runner is sandboxed and has none, which is why everything goes
  through testmanagerd. A patched WDA cannot invent a new injection channel;
  it can only reshape what testmanagerd offers. **Verdict: 做不到 (without
  those entitlements).**
- **One undocumented lead:** `XCPointerEventPath` has
  `initForEventStream:frame:speed:` and an `eventStream` property
  (`PrivateHeaders/XCTest/XCPointerEventPath.h`). What an "event stream" is
  there is undocumented and WDA never uses it. 没查到 any public
  documentation or use. **Verdict: 不清楚.**

What a patched WDA *can* do for sure: take the chunked-stream idea (F1) and
move it into WDA, closer to the phone — e.g. a route that keeps a gesture
session and plays each arriving segment as its own short record without Pinky
paying an HTTP round trip per chunk, or scheduling a segment's record before
the previous one finishes. That lowers latency but every segment still ends
with the finger up, so it is a better F1, not real touch.

## 4. What the change means for the user

- **Rebuild and reinstall the helper once per patched WDA version.** The user
  already does this: the helper is built with a free Apple ID (7-day profile;
  the current one runs out 2026-11-17 15:45 +08:00). Pinky already reads the
  profile's expiry off the phone (misagent, `internal/driver/ios/wda_profile.go`)
  and warns in the checkup (`internal/driver/ios/checkup.go:250-281`,
  `checkHelper`), so an expired patched helper shows up the same way an expired
  unpatched one does. With Sideloadly the bundle id keeps the
  `WebDriverAgentRunner` suffix (it must, for `pickWDARunner` to find it).
- **A patched WDA is a fork.** The patch itself is small (one command file +
  one route, mirroring `FBTouchActionCommands`), but appium/WebDriverAgent
  moves often, and every upstream update means re-merging and re-signing. The
  user would install a Pinky-patched WDA instead of the upstream one.
- **How Pinky finds and enables the route** (no code needed, just the seams):
  - Picking the runner: `pickWDARunner` (`internal/driver/ios/wda_run.go:83-107`)
    already prefers a runner whose bundle id, name or executable contains
    "pinky" — a patched build branded `Pinky.WebDriverAgentRunner.xctrunner`
    wins over upstream automatically, and an upstream one still works as a
    fallback.
  - Probing: after the session is created (`wda.go:300-324`, where Pinky
    applies `wdaSettings`), send one request to the new route (say
    `POST /wda/pinky/liveTouch`). A WDA without it answers with a WebDriver
    error (`wdaError`, `wda.go:214-221`) — probe once, remember the answer,
    and fall back to the F1 chunked gestures when it fails. `wdaClient.do`
    already retries once on a lost session, so the probe rides the normal
    plumbing.
  - Surfacing: `checkHelper` (`checkup.go:250-281`) reads `/status`'s build
    version (`wda.go:243-250`); a patched WDA could put a marker there so the
    checkup can say "live touch: yes/no" next to the profile expiry.
  - Nothing in `startWDA`/testmanagerd changes: the helper's env (`wdaEnv`,
    `wda_run.go:33-39`) only binds the two ports today, and the new route
    needs nothing more.

## 5. Prior art

**Conclusion: nobody has done real-time touch through WDA; the HID channel
that bypasses it exists in go-ios and pymobiledevice3, but the phone refuses
it below iOS 27 — so it cannot replace WDA here.**

- **appium/WebDriverAgent**: searched issues and PRs for "live touch",
  "touch streaming", "continuous touch", "pointerMove duration 0", "separate
  pointerDown pointerUp". Nothing that streams touch. The nearest neighbours:
  issue #496 (open, asks for smoother touch via simulated mouse events) and
  PR #1306 (`/wda/absolute-actions` — still whole-record synthesis).
- **facebook/idb** (now meta/idb): its direct HID injection
  (`FBSimulatorControl/HID/SimulatorHIDEvent.swift`) is simulator-only. On
  real devices idb drives through its own XCTest runner — the same
  record-based synthesis as WDA. No real-device touch channel.
- **danielpaulus/go-ios**: `ios/hid` injects real touch reports
  (`ios/hid/digitizer.go`) over CoreDevice's
  `com.apple.coredevice.hid.universalhidservice` — but the package header
  says it plain: "iOS 27+. an iOS 18 device answers CoreDeviceError 9021, and
  below 27 the service is not in RSD at all."
- **doronz88/pymobiledevice3**: same channel
  (`pymobiledevice3/remote/core_device/hid_service.py`, with
  `tests/remote/core_device/test_hid_touch_reports.py`), same constraint; its
  generic Indigo HID path is unfinished (the Mercury envelope for
  keyboard/digitizer is not reverse-engineered; `dtuhidd` logs "Resetting
  gesture state then canceling").
- Pinky's own code agrees: `internal/driver/ios/version.go:39-51` records that
  iOS 17.4-26 refuses the screen stream and HID input (error 9021), and
  `internal/driver/ios/hid.go:22` uses the same `universalhidservice` on the
  CoreDevice path (iOS 27+).

## Recommendation

- **A — keep and finish the F1 chunked-gesture scheme.** Work: what F1 is
  already doing, plus tuning on the real phone. Risk: low. User cost: none
  beyond the WDA already installed. Ceiling: each segment still presses and
  lifts the finger, so latency cannot go below roughly one segment's round
  trip, and apps that care about a *continuous* touch (drawing, some games,
  press-then-drag) still see the finger come up between segments.
- **B — patched WDA with a live-touch route.** Work: a small WDA patch and a
  Pinky probe/fallback (seams in §4). Risk: the core premise is unproven —
  XCTest offers no event-by-event injection, and whether a finger can stay
  down across two records is 不清楚. Ongoing cost: a fork the user must keep
  building and re-signing; upstream merges. Only worth starting after a
  one-hour spike on the phone (a record that never lifts, then a second
  record) answers the §3 question. If the spike fails, B degenerates into
  "F1 inside WDA", which saves one HTTP hop per chunk at the cost of a fork.
- **C — bypass WDA with HID touch events.** Dead below iOS 27: the phone
  refuses the `universalhidservice` (error 9021; not in RSD at all below 27),
  and idb has no real-device HID channel either. Everything that does this
  (go-ios, pymobiledevice3, Pinky's own CoreDevice path) works only on iOS 27+,
  where Pinky already uses the native path and WDA is not needed.

**Recommendation: A now.** It needs no helper change, no fork, no new user
steps, and F1 already removes most of the lag. Buy the one-hour spike for B's
premise when a phone is next free; if a held touch survives across records,
B becomes worth building on top of A as the fallback. C stays for iOS 27+,
where it already exists.
