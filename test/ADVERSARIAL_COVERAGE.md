# Adversarial / Data-Loss Coverage Map

Each data-loss invariant from `docs/design/2026-10-17-delete-safety.md` §6 mapped to the test(s) that prove it, so
the "never lose a frame" guarantee is auditable at a glance. Run the whole matrix
with `go test -race ./...`.

| # | Invariant | Proven by | Package |
|---|-----------------|-----------|---------|
| 1 | At-rest verify: a copy whose cache-bypassing read-back hash disagrees with the streamed hash is NEVER filed; the device original is left intact for re-pull. | `TestPullCorruptAtRestNeverFiles` | `internal/rig` |
| 2 | A streamed byte-count that disagrees with the listed size discards the staging temp and files nothing. | `TestPullSizeMismatchDiscardsTemp` | `internal/rig` |
| 3 | Delete integrity: a three-way (device / recorded / NAS re-hash) check failure at delete time HOLDS the frame and never removes the device original. | `TestDeleteCorruptNASHoldsNoDelete` | `internal/rig` |
| 4 | Delete TOCTOU: a source overwritten with new, never-verified content between the delete-pass hashes and the final delete is NOT deleted. | `TestDeleteSourceOverwrittenBetweenVerifyAndDeleteIsNotLost` | `internal/rig` |
| 5 | A both-sides cull (source gone AND NAS copy gone) reaches a terminal `deleted` state (so it stops spinning) and alerts frame-lost at WARN, not critical. | `TestDeleteDoublyVanishedConverges` | `internal/rig` |
| 6 | Idempotent re-pull adopts the existing NAS copy without restarting the grace clock or duplicating on disk. | `TestFileIdempotentRePullPreservesPulledAt` | `internal/rig` |
| 7 | Two different frames sharing a canonical name (single rig) are BOTH kept under distinct names; the clash raises rot-clash. | `TestFileDifferentContentSameNameBothKept` | `internal/rig` |
| 8 | **Cross-rig concurrent filing**: two rigs filing the same cal name at the same instant with different content — via a REAL atomic-exclusive rename over one shared localfs — both survive under distinct names; neither overwrites the other. | `TestCrossRigConcurrentFilingNoLoss` | `internal/rig` |
| 9 | Foreign identity marker refuses ALL pulls and deletes on that share (never touches another rig's data). | `TestIdentityForeignRefusesAndAlerts` | `internal/rig` |
| 10 | Missing routing-critical value quarantines the frame; a quarantined/unclassified frame is NEVER deletion-eligible. | `TestQuarantineUnclassifiedNeverDeletable` | `internal/rig` |
| 11 | A malformed header / unexpected value on one frame yields a failed row + alert, never a panic that kills the worker (which would stop the rig draining). | `TestPerFileRecoverYieldsFailedRow`, `TestReclassifyPanicDoesNotKillWorker` | `internal/rig` |
| 12 | VerifyRead hashes EXACTLY `size` bytes, discarding the sector-aligned tail past a non-sector-multiple FITS length. | `TestVerifyReadDiscardsTail`, `TestVerifyReadTailSizes` | `internal/localfs` |
| 13 | RenameNoReplace is a genuine atomic-exclusive rename: it fails distinctly (`ErrExist`) when the target exists rather than replacing it. | `TestRenameNoReplace_Exists`, `TestRenameNoReplace_Success`, `TestRenameNoReplace_Race` | `internal/localfs` |
| 14 | Adversarial OBJECT / INSTRUME values sanitize to a safe path component or collapse to a quarantine signal — no path traversal, no reserved-name filing. | route/sanitize table tests | `internal/route`, `internal/sanitize` |
| 15 | **Never-block**: with a deliberately slow/hung SMB device, the HTTP/UI read path (`/healthz`, `/events`, `/api/alerts`, `/api/config`) stays fast — it reads only the in-memory Store + StatusHub, never SMB (`docs/design/2026-10-17-architecture.md` §3). Measured max latency stays ~2 orders of magnitude below the 2s device delay. | `TestHTTPReadPathNeverBlocksOnSlowSMB` | `test/neverblock` |
| 16 | A removable medium that is inserted but drained reads **safe to power off**, while a genuinely absent one still reads **unknown** — both sides of the H5 empty-mount ambiguity, so neither can be fixed by breaking the other. | `TestSafeToPowerOffDrainedRemovableIsSafe`, `TestMediumPresenceDrainedStickIsPresent`, `TestMediumPresenceAbsentStickIsAbsent`, `TestPullRemovableAbsentMediumQuiet` | `internal/rig` |
| 17 | Presence never widens delete permission: an unadopted medium authorizes no deletes, and a foreign marker still refuses cleanup and alerts even though the stick reads present. | `TestUnadoptedMediumNeverAuthorizesDelete`, `TestMediumPresenceForeignIsPresent`, `TestMediumUsableSwapDetected` | `internal/rig` |
| 18 | With no persisted media id there is no prior adoption to protect, so a marked medium is never judged foreign. A marker at the canonical location adopts its EXISTING identity in place (state-loss recovery; two installs sharing a stick do not fight over it), while a lingering legacy-root marker still mints a fresh id so a reset is not undone. | `TestMediumUsableNoPersistedIDReadopts`, `TestMediumUsablePostResetRootMarkerReadopts` | `internal/rig` |
| 19 | A deleted rig leaves no per-share `mediaid:` key behind, so a reused rig id cannot judge a genuinely new stick foreign. | `TestDeleteRigClearsMediaID` | `internal/web` |
| 20 | An unreadable destination volume fails at CYCLE level in BOTH the pull and delete passes, not per frame: one WARN, one deduped fleet-wide alert, and the truncated pass is not published as an authoritative pending snapshot. | `TestDestinationUnavailableAbortsCycleAndAlertsOnce`, `TestDestinationUnavailableDeletePassDoesNotFlood` | `internal/rig` |
| 21 | A share root that cannot be listed reads NOT present, so an unreadable medium can never satisfy the safe-to-power-off verdict — the presence oracle has no fail-open branch. | `TestMediumPresenceUnreadableRootIsNotPresent` | `internal/rig` |
| 23 | An absent removable share is excluded from the published per-share free space and from the tightest-share capacity math — with no medium the ASIAIR exports its OWN rootfs through that share, which would otherwise publish a phantom figure and raise a spurious device-disk-low. The safe verdict still treats any absent share as unknown. | `TestAbsentRemovableShareExcludedFromCapacity`, `TestSafeToPowerOffDrainedRemovableIsSafe` | `internal/rig` |
| 24 | device_disk_low auto-acks when the tightest share is roomy again; without it the warning stands forever once raised. | `TestDeviceDiskLowAutoAcksOnRecovery` | `internal/rig` |
| 25 | An alert raised by a previous process whose condition is already clear at startup is still retired: the resolving-transition latches start "possibly raised", so a restart cannot strand an alert that only auto-ack can clear. | `TestStaleAlertFromPreviousProcessAutoAcks` | `internal/rig` |
| 26 | The unacked-alert badge is seeded from the store at startup, so a restart does not zero it while unacked alerts remain. | `TestStartSeedsUnackedAlertCount` | `internal/manager` |
| 27 | A directory large enough that its TRANS2 FIND response fragments across SMB1 messages enumerates COMPLETELY. A short listing returned with no error is indistinguishable from a drained device and would produce a false "safe to power off"; only a real server fragments, so this is proven against the Samba container with an exact count. | `TestLargeDirectoryListsCompletely` | `test/integration` |
| 28 | The AppleDouble twin ("._<name>") of a frame the delete pass proved and removed is swept with it, while an orphan twin whose parent Commander never owned is left alone. | `TestDeleteSweepsAppleDoubleTwin`, `TestDeleteLeavesOrphanAppleDoubleAlone` | `internal/rig` |
| 22 | Fleet-wide destination alerts (NAS low / unreadable, raised with no rig id so one shared volume yields one row) auto-ack when their condition clears, and stand while it holds. Without a fleet-scoped ack the rig-scoped matcher can never match an empty rig id, so they would stand forever. | `TestFleetWideAlertsAutoAckOnRecovery`, `TestNASDiskLowStaysWhileSpaceIsLow` | `internal/rig` |
| A1 | An empty microSD (TF) slot is treated as an absent removable slot — excluded from capacity, raising no device-disk-low, logged at Debug not WARN — through the real not-found listing path the Udisk slot takes. | `TestEmptyTFSlotTreatedAsAbsent` | `internal/rig` |
| A2 | An empty removable slot (TF or Udisk) still suppresses **safe** — H5 holds; only a drained-but-inserted medium reads safe. | `TestPullRemovableAbsentMediumQuiet`, `TestSafeToPowerOffDrainedRemovableIsSafe` | `internal/rig` |
| A3 | A populated TF share lists, pulls, and counts like any fixed share — the removable-slot split does not touch it. | `TestPopulatedTFShareUnchanged` | `internal/rig` |
| A4 | The delete-pass media-identity gate does not apply to TF: a TF device deletion is not gated on a media marker (`isRemovableShare` stays Udisk-only). | `TestTFDeleteGateUnchanged` | `internal/rig` |
| A5 | `isRemovableSlot` matches `TF Images`/`Udisk Images`, rejects `EMMC Images` and incidental substrings (`catfish`/`outfit`); `isRemovableShare` still matches Udisk only. | `TestRemovableSlotClassification` | `internal/rig` |
| B1 | Each `power_off_reason` is set for its Unknown condition in the documented priority (paused > offline > missing_on_device > removable_absent > listing_failed), and is empty whenever the verdict is not Unknown. | `TestPowerOffReasonPriority` | `internal/rig` |
| C1 | A unit-less duration in a config PUT or a rig create/update — which fails inside `json.Decode` before `Validate` runs — returns a `problems[]` entry naming the offending field with a unit hint, not the generic body error. | `TestUnitlessDurationNamesField` | `internal/web` |
| C2 | The JSON/API path rejects a bare-number duration (surfaced per-field), while `UnmarshalYAML` stays lenient (accepts bare seconds) so an existing hand-edited `config.yaml` still loads — a deliberate asymmetry. | `TestDurationUnitJSONRejectsYAMLLenient` | `internal/model` |
| C3 | No backend field silently regresses to the banner: every `model.Config` Duration is forced into the form-editable allowlist or the not-form denylist, and the validator's full emitted `Field` set must equal a checked-in fixture the web mapper is proven to cover. | `TestDurationFieldsCategorized`, `TestValidatorFieldFixture` (`internal/config`); `uncoveredValidatorFields` coverage module (`web/src/util`) | `internal/config`, `web` |
| C4 | A validation error on a mapped field renders inline at that input on Settings and the rig editor (banner only for genuinely unattached problems). | Manual browser pass | `web` |
| D1 | A disabled share is ignored where the rig acts: never mounted, listed, pulled, counted toward capacity/verdict, or cleaned/restored by the delete/restore passes (aggregate store counts still include its rows — harmless to the verdict). | `TestDisabledShareFullyIgnored`, `TestDeleteAndRestoreSkipDisabledShare` | `internal/rig` |
| D2 | An existing config with no `disabled` field loads every share enabled (`Disabled` zero value = enabled, so no migration). | `TestSourceDisabledDefaultsEnabled` | `internal/model` |
| D3 | `RigStatus.EmptySlots` publishes only the structurally-confirmed-empty branch, not the broader `absentShares`: a frame-bearing share hit by a transient read error is in `absentShares` (verdict suppressed) but NOT in `EmptySlots`, so the disable guard cannot strand its frame. | `TestStructurallyEmptySlotSetExcludesTransient` | `internal/rig` |
| D4 | The save handler refuses to disable a share (including create/re-add/config-PUT introductions) unless the rig is online, not paused, `ListingTS` is within one poll, and the share is in the structurally-confirmed-empty set — returning a `Problem` on `rigs[<id>].sources` otherwise. An already-disabled source passes untouched. | `TestDisableGuardStructurallyEmptyOnly`, `TestDisableGuardCreateAndReAdd` | `internal/web` |

## Notes

- Items 16–20 cover removable-media presence and its guards
  (`docs/design/2026-10-17-device-identity-and-media.md` §7). Items 16 and 17 are
  a deliberate pair: 16 pins the reporting behaviour, 17 pins that the reporting does not loosen any
  delete gate.
- Items A1–A5 cover removable slots (`docs/design/2026-10-17-device-identity-and-media.md` §7.6) and B1
  the power-off reason (`docs/design/2026-10-17-frame-lifecycle.md` §14.3).
  A1+A2 are the load-bearing pair, mirroring items 16/23: they pin that an empty TF slot raises
  nothing (A1) without weakening the absent-slot verdict (A2), while A4 pins that the removable-slot
  split does not extend the delete-pass marker gate.
- Items D1–D4 cover per-share disable (`docs/design/2026-10-17-device-identity-and-media.md` §8.3). D3 and
  D4 are the load-bearing pair: D3 pins that the published empty-slot set excludes a
  transient-error share, and D4 pins that the save guard authorizes a disable only
  against that structural set, so neither a count-based nor a transient-error hole can
  strand an un-pulled frame into a false "safe".
- Items C1–C4 cover per-field validation surfacing (`docs/design/2026-10-17-architecture.md` §8.8).
  C3's non-duration coverage is fixture-enforced rather than registry-enumerable
  (the validator exposes a `Field` only by tripping its rule); the equality
  assertion against `internal/config/testdata/validator_fields.txt` — copied to
  `web/src/util/__fixtures__/` for the web half — is what forbids drift. C4 has no
  automated proof; the inline wiring is verified by a manual browser pass.
- The invariants above are proven at the seam boundaries (the rig worker against
  fakes, and — for the two properties that require genuine OS-level atomicity and
  cross-goroutine concurrency — against the real localfs and a full manager+web
  stack). In addition, `test/integration/` (build tag `integration`, run via
  `make integration`) exercises the REAL smb1client path against a Samba/NT1
  container: byte-exact SMB reads, the H2 empty-root listing, not-found
  normalization, marker round-trip, and delete — the coverage the in-memory fakes
  cannot provide.
- Benign counterparts (a vanished source with a good NAS copy deletes cleanly; a
  differing frame over an intact NAS copy re-pulls WITHOUT a false frame-lost
  alert) are covered by `TestDeleteSourceVanishedGoodNASDeletes` and
  `TestDecideDifferBenignThenPull` so the guards do not over-fire.

## Library reconciliation

Each invariant from `docs/design/2026-10-17-library-reconciliation.md`
§5, mapped to its proving test. Deleting a culled original (`docs/design/2026-10-17-library-reconciliation.md` §9)
means a device
original IS removed for an absent NAS copy, but only once every disqualifier in
`absentDeferReasons` is clear AND the device copy still hashes to what was
archived. No runtime flag enables or disables it; there is no `deletionEnabled`
var to flip.

| # | Invariant | Proven by | Package |
|---|-----------|-----------|---------|
| I1 | A frame confirmed missing is never re-pulled while the device holds same-size content, in any state except `failed`, including a `deleted` row whose source reappears. | `TestDecideNeverRepullsMissingFrame` | `internal/rig` |
| I2 | A `failed` row that is marked missing still retries on its normal backoff. | `TestFailedRetryUnaffectedByMissing` | `internal/rig` |
| I2a | A marked frame whose device mtime shifts without a size change is not re-pulled by the differ branch. | `TestDifferDoesNotResurrectOnWeakSignal` | `internal/rig` |
| I3 | A truncated NAS copy is still restored automatically, including replacing the short file; an absent one never is: it routes to the deletion authorization. | No dedicated test | — |
| I4 | A culled frame's device original is deleted only after its mark settles, never a pass earlier. | `TestCulledFrameDeletedAfterSettle` | `internal/rig` |
| I4a | Every disqualifier (not marked, settling, restore requested, re-file in flight, supersede in flight, root unconfirmed, outside roots) defers; only a frame clear of all of them is authorized. | `TestAbsentDeferReasons`, `TestRefileInFlightDefersDeletion` | `internal/rig` |
| I4b | A device copy whose content changed since filing is held, never deleted: it is not the frame that was culled. | `TestCulledFrameWithChangedDeviceContentIsHeld` | `internal/rig` |
| I4c | Migrating a store across the migration that arms the verified cull (`migrate5`, schema 4 → 5) re-stamps every standing missing mark, so no backlog is authorized at once. | `TestUpgradeV4ToV5RestampsMissingMarks` | `internal/store` |
| I5 | The authorization is re-established from fresh state after the device hash: a Restore, or the NAS copy reappearing, during the hash stops the delete. | `TestCulledFrameDeleteRechecksFreshStateAfterHash` | `internal/rig` |
| I6 | The delete pass performs no device hash for an absent NAS copy it is not authorized to act on, while still performing the media check and source `Stat` that precede it. | No dedicated test | — |
| I7 | An absence is concluded only from a successful listing (or a directory confirmed gone) **and** a successful confirming `Stat`; every error class concludes nothing, in the scan and in the delete pass. | No dedicated test | — |
| I8 | A name differing only by case is never marked missing. | `TestCaseFoldedNearMatchIsNotAbsence` | `internal/rig` |
| I9 | A root whose marker is mismatched, corrupt, unreadable, or missing while an id is recorded is skipped and is never adopted or rewritten automatically; only a root with no recorded id is linked automatically, and only on proof from a verified file. | `TestLinkLibrarySemantics`, `TestAutoLinkOnFiledFrame`, `TestAutoLinkFromExistingDataOnScan` | `internal/rig` |
| I10 | Linking adopts an existing parseable id without rewriting, overwrites only an absent or corrupt marker, and clears marks under the root when the recorded id changes. | `TestLinkLibrarySemantics` | `internal/rig` |
| I11 | A failing root does not prevent reconciliation of another, nor ack another root's alert. | No dedicated test | — |
| I11a | A relink clears marks, and acknowledgements, for **every** rig under that root, not just the relinking one. | `TestClearMissingUnderRootClearsAcknowledgement` (single rig); cross-rig scope by construction | `internal/store` |
| I11b | Arming a deletion class re-stamps the standing marks of that class, so no backlog accumulated before the class could act is authorized without a fresh settle window. Holds for the verified cull (I4c); the proven-archived unclassified class is the recorded exception (`docs/design/2026-10-17-library-reconciliation.md` §11.4). | `TestUpgradeV4ToV5RestampsMissingMarks` | `internal/store` |
| I12 | Containment is separator-anchored and case-folded, and a row is assigned to the longest configured root that contains it. | `TestUnderRootContainment`, `TestLongestRootAssignment` | `internal/store` |
| I13 | `SetFrameMissing` is idempotent: a re-observation never restarts a running settle clock, so a manual rescan cannot accelerate a deletion. | `TestMarkIsIdempotent` | `internal/rig` |
| I14 | A file that reappears has its mark cleared by the scan, by the delete pass's present branch, by the post-hash re-check, and by the restore phase before it would replace anything. | `TestReappearanceClearsMark`, `TestCulledFrameDeleteRechecksFreshStateAfterHash`, `TestReappearedUnclassifiedCopyNotDeletedAndMarkCleared` | `internal/rig` |
| I15 | `UpdateFrame` cannot clobber the reconciliation columns; `UpsertFrame` and `DismissFrame` clear all three (`missing_seen_at`, `restore_requested_at`, `missing_acked_at`). | `TestMissingColumnsLifecycle`, `TestDismissFrameClearsAcknowledgement`, `TestUpsertFrameConflictClearsAcknowledgement` | `internal/rig`, `internal/store` |
| I16 | A restore requested at any point before a missing frame's original is deleted prevents that deletion, including one arriving mid-pass, and holds it indefinitely once retries fail. A request on a frame whose copy turns out to be present is cleared, not honoured. | `TestCulledFrameDeleteRechecksFreshStateAfterHash`, `TestAbsentDeferReasons`, `TestRestoreHeldOnDeviceMismatch`, `TestHalfCommittedRestoreEscalatesAfterItsBound` (bounded retry, no give-up) | `internal/rig` |
| I17 | An interrupted truncated-copy replace leaves the frame deferred and retried, never marked and deleted. | No dedicated test | — |
| I18 | The restore phase never removes a NAS copy that verifies. | No dedicated test | — |
| I19 | A row carrying a missing mark is never pruned. | `TestPruneDeletedSkipsMissingRows` | `internal/rig` |
| I20 | A missing frame whose source is still listed does not pin pending work, **and** downgrades Safe to power off to Unknown with reason `missing_on_device`, asserted after `publishListingStatus` has run, and sets `MissingWarning`. | `TestCullDrainYieldsAndStaysSafe`, `TestPowerOffReasonPriority` (verdict and reason); `MissingWarning` has no dedicated test | `internal/rig` |
| I21 | A missing row whose `dest_path` falls under no configured root is never authorized, and the scan reports the count. | `TestAbsentDeferReasons` (authorization half); reporting has no dedicated test | `internal/rig` |
| I22 | The scan runs for an offline rig and for a `keep_originals` rig, and never runs for a paused one, including under run-now and an identity reset. | `TestScanSkippedWhenPaused` (pause half) | `internal/rig` |
| I23 | A both-sides cull raises `frame_lost` at warn, not critical, and reaches a terminal `deleted` state, so it is not re-selected every poll cycle forever. | `TestDeleteDoublyVanishedConverges` | `internal/rig` |
| I24 | Reserved; not used. | — | — |
| I25 | Acknowledge waives only the settle window; every other disqualifier (`root_unconfirmed`, `outside_roots`, `restore_requested`, `refile_in_flight`) still defers on an acknowledged frame. | `TestAcknowledgeDoesNotWaiveOtherReasons`, `TestAcknowledgedFrameSkipsSettleWindow` | `internal/rig` |
| I26 | An acknowledgement never outlives its missing mark: a fresh mark nulls a stale acknowledgement, while a re-observation of a standing mark preserves a live one. | `TestSetFrameMissingClearsStaleAcknowledgement` | `internal/store` |
| I27 | A confirm that never saw a challenge (a token never minted, replayed after success, expired, or scoped to another frame or rig) cannot delete. | `TestAcknowledgeFrameRejectsBadTokens`, `TestAcknowledgeFrameReplayRejected`, `TestAcknowledgeFrameExpiredToken` | `internal/web` |
| I28 | A group token binds its candidate ids, so a directory that grows between mint and confirm cannot widen the acknowledgement. | `TestAcknowledgeGroupChallengeAndConfirm` | `internal/web` |
| I29 | Acknowledge races Restore safely: Restore clears the acknowledgement, and an acknowledgement is refused while a restore is pending. | `TestRequestRestoreClearsAcknowledgement`, `TestAcknowledgeRefusesNonVerifiedAndRestorePending` | `internal/store`, `internal/rig` |
| I30 | A row outside the acknowledgeable set cannot be acknowledged, enforced by the SQL guard, the mint refusal, and the absence of a UI control. The per-frame set is proven-archived; the group set is `verified` only (I35). | `TestAcknowledgeMissingRefusesUnmarkedRestorePendingAndNonVerified` | `internal/store` |
| I31 | A culled delete resolves the row out of the scan, so the following scan does not re-mark it or raise a false `library_reconciled` alert. | `TestAcknowledgedCulledFrameDeletedAndDoesNotReappear`, `TestResolveCulledFrameRetiresRow` | `internal/rig`, `internal/store` |

The authorization invariants are the settle window (I4), every disqualifier (I4a), the
device-content proof (I4b), the arming re-stamp (I4c, I11b), and freshness at the moment of
action (I5, `docs/design/2026-10-17-library-reconciliation.md` §2.7). A row whose proof reads
"No dedicated test" is exercised by the wiring and by neighbouring tests but has no test that
pins it alone; I24 is reserved and unused.

## Culled-original drain for proven-archived unclassified frames

Each cull-drain invariant (`docs/design/2026-10-17-library-reconciliation.md` §5, §11) mapped to
its proving test. The drain makes the culled-original deletion cover any frame
Commander can prove was archived — a recorded `sha256` and a safely-on-NAS state
(`verified` **or** `unclassified`) — and every other gate applies to it unchanged. The
re-hash gate compares the fresh device read to the `f.SHA256` device anchor (never
`archiveHash`); `held` is never deletable; normal cleanup stays `verified`-only; and
group acknowledge stays `verified`-only. I32 (device re-hash) and I33 (reappeared copy)
get integration tests (real OS/SMB behavior, not fakes), run via `make integration`.

| # | Invariant | Proven by | Package |
|---|-----------|-----------|---------|
| I32 | A device original is cull-deleted only for a proven-archived frame (`sha256` recorded, state `verified` or `unclassified`). The gate compares a fresh device hash to the row's current device anchor `sha256`, never the archive anchor; a changed device copy is held, never deleted; `held`, `deleted`, `failed` and `dismissed` are never cull-deleted. | `TestCullDrainDeviceRehashByteExact` (integration, real SMB), `TestCulledUnclassifiedFrameDeletedAfterSettle`, `TestCulledUnclassifiedWithChangedDeviceContentIsHeld` | `test/integration`, `internal/rig` |
| I33 | Only a **missing** `unclassified` frame is deletion-eligible; a non-culled one is never in the query, and a reappeared copy is skipped after its mark is cleared. | `TestCullDrainReappearedCopyRealFS` (integration, real filesystem), `TestReappearedUnclassifiedCopyNotDeletedAndMarkCleared`, `TestDeletionEligibleScopesUnclassifiedByRigAndGrace` | `test/integration`, `internal/rig`, `internal/store` |
| I34 | Every `absentDeferReasons` gate, and the device-hash check, apply to an `unclassified` cull identically to a `verified` one. | `TestCulledUnclassifiedRespectsDeferReasons`, `TestAbsentDeferReasons` | `internal/rig` |
| I35 | Per-frame acknowledge is restricted to the proven-archived set; group acknowledge stays `verified` only; `held` and `deleted` are not acknowledgeable at the worker, the mint, the SQL guard or the UI. Acknowledge waives only the settle window, so the acknowledgeable set is a superset of what is cull-deletable, which additionally requires `sha256`, grace, a confirmed root and a matching device hash. | `TestServiceAcknowledgeAcceptsUnclassifiedRefusesHeld`, `TestAcknowledgeFrameChallengeAcceptsUnclassified`, `TestAcknowledgeMissingUnclassifiedPerFrameVsGroup`; the UI layer by a manual pass | `internal/rig`, `internal/web`, `internal/store` |
| I36 | A culled `unclassified` frame does not count toward the never-deletable backlog alert; a non-culled one, and every `held` or `failed` frame, does. A culled frame under an unconfirmed root never drains yet is excluded too, as verified culls are. | `TestCountNeverDeletableExcludesCulledUnclassified` | `internal/rig` |
| I37 | The deletion query always applies `rig_id`, grace and a recorded `sha256` across **both** state branches: no cross-rig or pre-grace frame is ever returned. | `TestDeletionEligibleScopesUnclassifiedByRigAndGrace` | `internal/store` |

The drain needs no schema migration of its own; `TestFreshDBIsSchema10AndHasOverlayColumns`
in `internal/store` pins the fresh schema. The UI layer of
I35 (the per-frame Acknowledge control and the "Acknowledged" pill covering
unclassified frames, and the culled frame excluded from the unclassified view) has no
automated proof; it is verified by a manual browser pass (`docs/design/2026-10-17-library-reconciliation.md` AC6/AC9).
