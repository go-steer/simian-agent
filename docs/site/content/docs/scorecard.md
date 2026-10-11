---
title: "Reading an evaluation: the scorecard"
linkTitle: "Scorecard"
weight: 27
description: "Combine simian-eval runs into one scorecard document and read it in the web UI: scenarios by subjects, what was injected, whether it landed, and what each subject reported. No server needed."
---

`simian-eval` runs a scenario pack against one subject and leaves a run
directory (`audit.log` and `run.json`). A **scorecard** puts several of those
runs side by side — k8s-lookout, an agent, the noop floor, two versions of the
same agent — as one grid of scenarios by subjects, and keeps the evidence
behind every cell: the fault Simian injected, whether it landed, the ground
truth, and each subject's report.

The scorecard is one JSON file. It needs no server to read: open it in any
Simian web UI, in `simian web`, or by opening the page and dropping the file
on it.

## Making one: `simian-eval report`

```bash
simian-eval report \
  --run runs/dataplane-lookout \
  --run runs/dataplane-agent \
  --run runs/dataplane-floor \
  --out scorecard.json
```

Each `--run` is a column. Every number is computed by the same code that
printed that run's own scorecard (`simian evaluate`), so a cell matches the
run's text scorecard exactly; the report only arranges them. Each run is
scored against the pack its audit log names; pass `--pack DIR` for a pack
that is a directory rather than a built-in one. Without `--out` the document
goes to stdout.

Runs of different packs may be combined: the grid groups scenarios by pack
and says so. A run that did not include a scenario has an empty cell there
("not in that run"), which is different from "not measured".

| Flag | Default | Notes |
|---|---|---|
| `--run` | (required) | A run directory holding `audit.log` and `run.json`. Repeatable. |
| `--out` | stdout | Where to write the scorecard. |
| `--pack` | (as recorded) | Packs to score against, matched to each run by the pack name in its audit log. Built-in packs need not be listed. |
| `--floor` | (lookout) | Subjects that are the floor. Default: any subject named `lookout` or `k8s-lookout`, with or without a version suffix (a `lookout:` subject is named after its binary, e.g. `lookout-v0.20.0`). `--floor ""` for none. |
| `--min-efficacy` | 0.8 | Below this fraction of faults landing, a run is flagged as measuring the harness, not the subject. |

## Two rules the grid keeps

**A fault that did not land is *not measured*, never zero.** If a scenario's
efficacy gate failed in a run, the cluster was never broken the way the
scenario says, so nothing was measured about the subject there. The cell shows
a hatched dash marked *not measured*, and the drill-down says which gate
failed and what it observed. A run in which fewer than `--min-efficacy` of the
faults landed carries a warning: its numbers describe the harness.

**The deterministic detector is the floor, not a peer.** k8s-lookout's column
is marked *floor*. A subject that scores below it on a scenario is flagged
*below floor* with the floor's score: it had a tool that found more and did
not use it, which calls for a different fix from a wrong diagnosis. The noop
subject (`noop:`) is the zero baseline and is never flagged.

Each cell shows **recall** — the measure the text scorecard leads with — and,
on a healthy control, where there is nothing to recall, **hallucinated
fault**. Time to detect sits under it. Hover a cell for every measure with its
reason; pick a scenario for the drill-down.

## Reading one

- **In any Simian web UI.** Press **Scorecard** in the top bar, then drop the
  file on the page or pick it. **Live** goes back; the live view keeps
  running behind it. A link opens one directly:
  `…/ui/?scorecard=<url>`, where the URL is on the same site or `https://`.
- **With `simian web`.** `simian web --scorecards DIR` serves the `*.json`
  files in `DIR`, read-only, at `/scorecards/`, and the page lists them. With
  no controller to watch, the page opens in Scorecard mode.

  ```bash
  simian web --addr :8080 --scorecards ./scorecards
  # then open http://localhost:8080/ui/
  ```

- **With nothing running.** Open `index.html` from Simian's web assets
  (`pkg/webui/web/`) straight from disk and drop the file on it.

The scorecard pane holds the runs (subject, pack, cluster, when, how many
faults landed) and the grid; the scenario pane holds the drill-down: the
prompt the subjects were given, the injected fault (kind, targets, spec,
duration, its efficacy gates), the ground truth, whether it landed in each
run, and each subject's report side by side with time to detect and time to
remediate. In the room they are two panes in place of the live ones; in the
flat layout, two cards.

## The document

A scorecard is self-describing JSON (`format: "simian.scorecard"`,
`version: 1`):

| Field | What it holds |
|---|---|
| `generated_at`, `generator` | When and by which `simian-eval` it was made. |
| `headline`, `control_headline`, `measures` | Which measure the cells show, and every measure's unit. |
| `packs`, `mixed_packs` | The packs the runs used. |
| `floor` | `rule` (`auto`, `flag` or `none`), the floor `subjects`, and the run IDs that are the floor. |
| `runs[]` | Per column: `id`, `label` (the directory's name), `subject`, `pack`, `cluster`, `started_at`/`ended_at`, `floor`, `null_subject`, the harness's numbers (`scenarios`, `manifested`, `inject_failures`, `efficacy_rate`, `below_min_efficacy`), `means`, and `evidence` (other files in the run directory, such as transcripts). |
| `scenarios[]` | Per row: `id`, `name`, `pack`, `prompt`, `control`, `headline`, `faults` (as the pack declares them), `ground_truth`, and `results` keyed by run ID. |
| `scenarios[].results.<run>` | `manifested` and `inject_error` (efficacy), `scores` (every measure, as the run's scorecard has them), `value` (the headline; `null` when not measured), `floor_value`/`below_floor`, the timestamps, the subject's `report` and `subject_error`, and `faults`: what the audit log says happened to each fault, with its efficacy records. |

The cluster is what `simian-eval` recorded: the kubeconfig context or the kind
cluster's name. Runs from before it recorded one show none. A subject's report
is kept whole up to 200 findings, and its error text up to 4 KB.

Subject errors are copied verbatim, so a scorecard can carry whatever a
subject wrote to stderr — a kubeconfig context name, for instance. Read it
before sharing a scorecard outside the team.
