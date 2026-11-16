# K4 (the knowledge volume): parked, 2026-11-16

The operator parked this branch on 2026-11-16 after an advisor review. It is plumbing that works
(the volume, `VERSION`, exact keys, `loaded`, the deploy copy), not something to deploy. The
knowledge work follows fleet-eval-data `projects/knowledge/STAGE1-PLAN.md`: K4 comes after K3 (bug
fixing reads the docs) shows an effect, and its maps are generated from each repo's `llms.txt`, with
sections, after an RFC addendum.

## Before this branch can be merged

1. **Replay is impossible.** `signal-fleet/mvp/triage.py`: `name == "knowledge" and table is None`
   sends a knowledge call in any replay (which always passes a probe table) to `probes.run_probe`,
   which answers `<TABLE-MISS>`, and `score.py` drops those runs. Serve knowledge whenever
   `knowledge.available()`; the volume at a pinned `VERSION` is the frozen table.
2. **Rollback breaks.** flotillad parses its config with `KnownFields(true)`; the previous image's
   `-check` fails on a `knowledge:` section, so `deploy.sh --rollback` cannot go back. The rollback
   step must remove the section, or the deploy must keep a config copy per tag.
3. **It cannot deploy dark.** `fleets/triage/cycle.yaml` declares `knowledge:`, so merging puts the
   tool into the live triage prompt. Declare it in `cycle-shadow.yaml` only until a replay passes.
4. **Untied declarations.** `.env` (`KNOWLEDGE_HOMELAB_PATHS`), `flotillad.yaml` (`paths`, the
   unused `volume`) and the compose volume are not checked against each other; a mismatch shows only
   as a read error at run time. Mount the volume on flotillad and have `-check` verify every map value
   exists there.
5. **Smaller:** the cut marker's count is wrong (`runtime/knowledge.py`); `PROMPT_SHA` now depends on
   the installation; a read during a deploy's folder swap can pair one version with the other's
   bytes; the dismissal gate does not check that `dismissal_evidence` names a probe, so a handbook
   page can be the real basis of a dismissal.

## Content

The homelab `docs/recipes/` pages are shell recipes for agents with a shell (`curl`, key files), not
what a triage model with six probes needs, and their sections are mostly far over the ~1,400
characters a read shows (GlitchTip 10k, Grafana 8k). Do not fill a `by_service` map from them. The
documentation examples (`docs/recipes/litellm.md`, `glitchtip.md`, `backup.md`) name files that do
not exist.

## The replay that has to pass first

The 23 labelled fixtures in fleet-eval-data `projects/signal-fleet-replays/reference`, two arms
(without and with knowledge, the folder built by `git archive` at one pinned commit), k=3, about
500-600 model calls (price stated before running): the EVAL.md bar (no false dismiss, escalate
≤5%) plus how often the tool is called, which probes it displaces, tokens per decision and dismissals
that cite the handbook. A free precheck first: read what the model would see for those services.
