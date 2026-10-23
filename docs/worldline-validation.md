# Worldline validation

This record contains synthetic-only checks. The live browser checks used only the authorized `demo` profile at `127.0.0.1:8790`; no real profile, transcript, or screenshot was read or saved. Temporary screenshots and JSON reports were kept under `/tmp`.

## Frontend and mocked browser

On 2026-10-23, `cd web && npm run typecheck && npm run build` passed. The synthetic Playwright flow in `scripts/worldline-synthetic-browser.py` passed all 17 checks:

- Source quotation and separate mention/effective-time display.
- Merge destination filtering by subject and category.
- A stale 409 keeps the correction draft, and retry submits the expected claim/action/version.
- Generation failure then retry reuses the request ID and displays completion.
- The default topic-thread view keeps mention chronology separate from fact validity and cautions that a shared topic does not establish a shared speaker.
- Undated claims remain in the separate unknown-time panel.
- Detail pagination appends a source lane absent from the initial preview.
- Zoom and pan controls, empty-search state, and 390px responsive layout.
- Topic search and pagination, and the preserved validity-time view.
- Validity-detail pagination retains its active search and status filters.
- Mention-axis and topic-lane alignment on desktop and mobile.
- Semantic suggestions are labeled “待核对”, show both source summaries, quotations, and conversation links, and opening either side sends no feedback mutation.
- Generation displays the returned model name and suggestion count, while an unconfigured engine is stated explicitly.
- An `insufficient_evidence` result is rendered as “无法判断，来源信息不足”, not as a positive relation.
- A thread with a nonzero semantic review count shows “N 处关系待核对”, without labeling the suggestions as fact conflicts.

The semantic UI fixture used the explicit mock name `synthetic/mock-engine`. It did not invoke JEV or any model/provider; this only validates rendering and review-only behavior. Real JEV configuration and a live model run are not established by this browser check.

## Live synthetic demo

The demo profile login and worldline render passed. `GET /api/worldline` returned HTTP 200. At the time of the check, the UI showed 2 macro topics, 19 preview lanes, 36 claims, and 21 unknown-time claims; no claims had an effective-time bar in the initial view. This is expected for the fixture: mentions appear on their separate track and are not promoted to effective dates.

Read-only detail pagination passed with HTTP 200 from `GET /api/worldline/topics/{id}`. An empty search filter also passed. No live feedback or correction mutation was sent.

Audio playback passed in the synthetic demo: an evidence item reported `available`, its audio endpoint returned HTTP 200 as WAV, and the browser advanced playback time to 1.81 seconds after two seconds with no media error. The clip duration was 3.5 seconds. The test used a headless Chromium audio sink; it verified media playback progress, not audible output from physical speakers.

Generation idempotency passed against the synthetic demo. Two POSTs with the same synthetic request ID returned HTTP 200, `complete`, and the same job ID. The history contained three job events after the first call and still three after the retry, confirming no duplicate events. This check intentionally wrote only those generation events to the approved demo profile.

The page-level generation button is covered by the mocked browser flow: a temporary failure followed by retry reuses the same request ID, then displays completion. No live feedback or correction mutation was sent.

## Cross-session mention timeline

The approved demo profile was used for a read-only browser smoke test. The thread list and all 8 first-page thread-detail requests returned HTTP 200; the page showed 8 concrete threads and 9 mention markers. The fact-validity view remained available, and the 390px page had no horizontal overflow. The axis and mention markers aligned with thread lanes on both desktop and mobile. The first thread in this demo fixture contained one dated statement, so the five-item preview and reveal behavior were exercised by the mocked browser flow rather than by this live fixture. A synthetic-only screenshot was kept at `/tmp/worldline-threads-demo.png` and is not part of the repository.
