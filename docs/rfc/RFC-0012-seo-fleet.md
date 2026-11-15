# RFC-0012 — The SEO fleet: Orrery's search presence, measured

**Status:** Proposed (2026-11-15). Step 5 of the [vision plan](../wip/vision-plan.md).
**Builds on:** [RFC-0001](RFC-0001-fleet-platform.md) (fleets in one platform; the SEO fleet named
there as RFC-0003, a number never used), [RFC-0004](RFC-0004-agent-pipeline.md) (detections, cases,
plans, autonomy levels), [RFC-0009](RFC-0009-post-incident-review.md) (a detection's `measure`).
**Inputs:** Orrery's SEO brief and review (`docs/wip/2026-11-08-seo-brief.md`,
`docs/wip/2026-11-08-seo-review/` in chipi/orrery) and its ADR-096 (indexable entity pages, the SEO
gate in its build).

## Motivation

Orrery (`www.orrerylearn.com`, 14 locales, about 12,000 indexable pages since ADR-096) fixed what a
build can check: titles, descriptions, canonicals, hreflang, structured data, sitemaps. Its `check-seo`
gate fails a build that breaks them. What a build cannot check needs data and time:

- what the live site serves, as opposed to what the build produced (Cloudflare's managed robots block
  disallowed Baiduspider without anyone deciding it; the GitHub Pages mirror outranked the real site);
- what people search for and where the site ranks, per locale (Search Console);
- where visits come from (Umami: search was 12.1 % of visits, 2026-08-16 to 11-08);
- whether a shipped change helped, 4 to 8 weeks later.

The brief asks for exactly that as recurring work: a report every week, proposals with evidence, every
change measured. Recurring work with evidence and a decision is what flotilla runs.

**Non-goals:** changing the site (the release work does that, on Orrery's release branch); paid tools
or paid APIs; link building; the mobile apps; sites other than Orrery in v1 (the design takes the
site as a parameter, so the photo sites and Close Listening can follow).

## Proposal

### 1. A fleet, its worker and its credentials

A fleet `seo` ("the digital marketing fleet", its first job SEO). One worker role, `marketing`, on the
mini, as `docs/architecture/deployment.md` already lists it: read-only, light (a crawl of a few
hundred URLs a day, a few API calls a week), so it fits the mini's role as orchestrator and service
platform. It runs the ops image's pattern: Python stdlib, no model in v1.

Credentials, each its own file in the worker's secrets volume, read by path:

| Source | Credential | Who makes it |
| --- | --- | --- |
| the live site | none | — |
| Umami | a view-only Umami user with access to the Orrery website only | I can (admin API), with the operator's go |
| Search Console | a Google Cloud service account's JSON key; the account added to the property as a Restricted user | the operator (Google console) |
| PageSpeed Insights (v2) | an API key restricted to that API | the operator |

The egress proxy allows only: `www.orrerylearn.com`, `chipi.github.io` (the mirror's noindex check),
the Umami host, and for Search Console `oauth2.googleapis.com` and `searchconsole.googleapis.com`.

### 2. The routines (v1)

| Routine | Trigger | Reads | Reports |
| --- | --- | --- | --- |
| `seo/site-check` | daily | `robots.txt`, the sitemap index and its 14 locale sitemaps, a fixed sample of routes per locale (home, the hubs, one entity page of each kind), the mirror's home | detections when the live site breaks what the build guarantees |
| `seo/traffic` | weekly | Umami: visits by referrer, search engine, landing path, locale, country, week over week | the week's numbers; detections on a drop |
| `seo/search-console` | weekly, once access exists | Search Console: queries and pages (clicks, impressions, CTR, position) per locale and country; sitemap acceptance; the URL Inspection API for the sample | the week's numbers; detections on opportunities, drops and exclusions |

`seo/site-check` needs no credential, so it is the first routine to run in shadow. It repeats on the
live site the checks `check-seo.mjs` runs on the build, for the sample: status 200, one title that is
not the bare brand, a description, a self canonical, complete hreflang pointing at pages that answer
200, no `noindex`, JSON-LD that parses; plus what only the live site shows: `robots.txt` allows
Googlebot, Bingbot and Baiduspider (the operator allowed Baidu on 2026-11-08), every sitemap answers
and lists URLs, the mirror serves `noindex`.

### 3. What a finding is

A finding is a detection, so it becomes a case with its evidence, as every other fleet's does. Each
detection carries a `measure` (RFC-0009 §3): the number, the threshold, the window.

| Kind | Target | From | Fires when |
| --- | --- | --- | --- |
| `seo_live_regression` | `site:orrerylearn.com/<path>` | site-check | a sampled page or `robots.txt` or a sitemap fails a check above; one case per check and path |
| `seo_traffic_drop` | `site:orrerylearn.com/<locale>` | traffic, search-console | a locale's search visits or clicks fall by more than a threshold week over week, with a minimum count so small numbers do not fire |
| `seo_opportunity` | `site:orrerylearn.com/<path>#<query>` | search-console | a query ranks 8 to 20 for a page with enough impressions (the brief's "cheap wins") |
| `seo_not_indexed` | `site:orrerylearn.com/<path>` | search-console | a sampled page is excluded, with Google's reason |

The weekly numbers are not findings. They go into the brief (§4) and into a history the fleet keeps
(§5), so a later finding can say "down from 410 to 260", not "down".

### 4. What it may do, and where it shows

- **v1: nothing outward.** Every routine runs at `shadow`; the cases are evidence for the operator.
  No plan, no action in the registry.
- **v2, propose:** a plan for an `seo_opportunity` or `seo_live_regression`: the page, the current
  title and description, the proposed ones in every locale, the evidence and the expected effect, as
  a draft PR to Orrery's release branch through `github.open_pr` (the bugfix fleet's action, once the
  GitHub App exists). That needs a model and Orrery's locale rules (native search terms, all 14
  locales), so it is its own step, and it never goes past `propose`.
- **Measure:** when such a PR merges, the case records the date and the query; a check 4 and 8 weeks
  later compares clicks, CTR and position and closes the case as kept or as a revert proposal. Until
  then a change is not done (the brief's rule).

In the brief: open SEO cases appear under **Needs you** like any case; a weekly **Search** section
(Mondays) carries the week's numbers per locale with the change from the week before.

### 5. Data it keeps

Weekly snapshots as JSON under the fleet's state directory (`docs/architecture/data.md`: what a fleet
collects stays in files the fleet owns). Parquet when the history is large enough to need it; 14
locales a week is not.

### 6. Tests

- site-check against recorded pages (a good page, each failure shape, a `robots.txt` that blocks
  Baiduspider, a mirror without `noindex`), so every detection kind has a case that fires and one
  that does not.
- traffic and search-console against recorded API responses, with the thresholds' edges.
- The e2e stack runs site-check against a local fixture site.

## Open questions (the operator's)

1. **The name.** Fleet `seo` with worker `marketing` (as proposed), or fleet `marketing` with
   `seo/*` routines?
2. **Search Console access.** You create the Google Cloud project, enable the Search Console API,
   make a service account and add it to the property as a Restricted user; I store the key file
   (mode 600, outside any repo) on the mini. Signing the token needs RS256, which the Python stdlib
   cannot do. Options: (a) the `openssl` command line in the marketing image (one Debian package);
   (b) `cryptography` pinned with hashes; (c) node's built-in crypto, as the bugfix image's
   `github_app.js` does. I recommend (a): smallest, no Python dependency.
3. **Umami.** A view-only Umami user for the Orrery website only (recommended), made through the
   admin API, instead of the admin password the hand reviews used.
4. **Cadence and thresholds.** site-check daily; traffic and search-console weekly (Search Console
   data lags 2 to 3 days). A drop: more than 30 % week over week with at least 50 visits or clicks
   in the earlier week. An opportunity: position 8 to 20 with at least 100 impressions in 28 days.
   Starting values, to be revised from the shadow weeks.
5. **The Search section** in the brief: weekly, as proposed, or only when there is a finding?

## Alternatives considered

- **An agent with the brief, run by hand every week** (as the 2026-11-08 review was). It worked once,
  but its numbers were not kept, nothing compared week to week, and the measurement rule depends on
  someone remembering. The routines keep the history and the clock.
- **GA4.** Out (the vision plan): Umami is the site's analytics.
- **A model in v1** to write the weekly report. The numbers are the report; a model adds value at the
  proposal step (v2), where wording in 14 languages is the work.
- **Running on the X3.** The work is light and read-only; the X3 is for heavy agentic work (RFC-0011).
  The v2 proposal step, with a model and a checkout, may move there.

## Discussion

- 2026-11-15 — drafted. Not found on the X3 or the mini: a Search Console key or a PageSpeed key
  (searched `~/.config` and the home directories on both); the brief records that both wait on the
  operator.
