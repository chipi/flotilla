# The seo fleet

The digital marketing fleet ([RFC-0012](../rfc/RFC-0012-seo-fleet.md)), on the `marketing` worker
on the mini. Code only, read only, in shadow: each finding opens a case; nothing goes out.

| Routine | Every | Reads | Opens |
| --- | --- | --- | --- |
| `seo/site-check` | day | the live sites | `seo_live_regression` |
| `seo/traffic` | week | Umami: visits and visits from search engines, against the week before | `seo_traffic_drop` |
| `seo/search-console` | week | Search Console: clicks against the week before, queries at 8 to 20, URL Inspection of the fixed pages | `seo_traffic_drop`, `seo_opportunity`, `seo_not_indexed` |

The two weekly routines report their numbers as summary lines; the brief written after their run
shows them under **Search** (their specs' `brief_section`), so the section appears once a week.

## The sites

`seo/sites.json`, one entry per site, baked into the marketing image:

| Key | Meaning |
| --- | --- |
| `name`, `base` | the site's name and its canonical origin (`https://www.orrerylearn.com`) |
| `pages` | paths always checked (default `/`); their hreflang alternates are fetched too |
| `sample_prefixes` | per prefix, the first URL of the sitemap below it (sorted) is checked as well |
| `brand` | a title equal to it alone is a finding |
| `bots` | crawlers robots.txt must let in at `/` (default Googlebot and Bingbot) |
| `json_ld` | `true` (every page) or the paths that must carry JSON-LD: `/` is the home page alone, a path ending in `/` the pages below it |
| `hreflang`, `locales` | check the alternates; fewer than `locales` is a finding |
| `retired` | URLs of hosts that must answer 404/410 or serve `noindex` |
| `umami` | the site's Umami website ids (summed) |
| `search_console` | its Search Console property; a property holding several sites is filtered to this site's host |

A new site: an entry here, the host in `egress-seo-acl.yaml`, its Umami website moved into the
`seo-read` team, and the service account added to its Search Console property.

## What a finding looks like

Target `site:<check>/<host><path>`, e.g. `site:canonical/airandspacehistory.com/`. site-check's
checks: `robots`, `robots-<bot>`, `sitemap`, `status`, `redirect`, `html`, `noindex`, `title`,
`description`, `canonical`, `json-ld`, `hreflang`, `retired`. traffic's: `visits`, `search-visits`.
search-console's: `clicks`, `opportunity` (`<path>/<query>`), `index`. The case's data carries the
URL and a `measure` (the value found and the one expected). A repeated finding attaches to its open
case. These kinds have no acting routine, so their cases wait in Needs you until a person closes them:
flotillad clears an incident on its own only for a case closed as a shadow plan (an acting routine's),
so a check that passes again does not close its case.

The thresholds (`seo/common.py`, the operator's starting values): a drop is more than 30 % below the
week before, from at least 50; an opportunity ranks 8 to 20 with at least 100 impressions in 28 days
(the 10 largest per site). Search Console's window ends 3 days back, its data's lag.

## Install (once, on the mini)

All in `FLOTILLA_CONFIG` (`~/flotilla/config`), each secret created mode 600 and never printed.

1. **Worker token:** `worker-marketing.token`, 32 random bytes as hex.
2. **`flotillad.yaml`:** under `workers:`,
   `marketing: {url: "http://worker-marketing:8081", token_file: /run/flotilla/worker-marketing.token}`.
3. **`egress-seo-acl.yaml`:**

   ```yaml
   version: v1
   services: []
   default:
     project: flotilla-seo
     action: enforce
     allowed_domains: [www.orrerylearn.com, orrerylearn.com, chipi.github.io, www.humancopilot.eu,
       humancopilot.eu, evals.humancopilot.eu, www.devwhateverops.com, devwhateverops.com,
       www.markodragoljevic.com, markodragoljevic.com, airandspacehistory.com, www.airandspacehistory.com,
       umami, oauth2.googleapis.com, searchconsole.googleapis.com]
   ```

4. **`smokescreen-seo.yaml`:** a copy of `smokescreen.yaml` whose `allow_ranges` is Umami's docker
   network only (`docker network inspect umami_default`; `172.24.0.0/16` on the mini), so this proxy
   reaches no other private address.
5. **The read credentials,** in `FLOTILLA_HOME/marketing/` (`~/flotilla/marketing/` on the mini), mode
   600; `secrets.sh` copies them into the worker's volume:
   - `umami.env`: `UMAMI_USERNAME=flotilla-seo` and its `UMAMI_PASSWORD` (a view-only Umami user in
     the team `seo-read`, which owns the sites' websites; the compose file gives the URL);
   - `gsc.json`: the key of the service account `flotilla-seo@flotilla-seo.iam.gserviceaccount.com`,
     a Restricted user on each property. The token is an RS256 JWT signed with `openssl` and
     exchanged at `oauth2.googleapis.com` for a `webmasters.readonly` token.
6. **`FLOTILLA_HOME/.env`:** add `seo` to `COMPOSE_PROFILES`.
7. Deploy as usual (`deploy/mini/deploy.sh`): `secrets.sh` fills the worker's volume.

## Run it by hand

`docker exec -i -e FLOTILLA_TOKEN_FILE=/dev/stdin flotillad /usr/local/bin/flotilla run seo/site-check
< ~/flotilla/admin-marko.token`, then `flotilla runs seo/site-check`. Outside flotilla, the script
alone: `python3 seo/site_check.py` (one line per site, one per finding; nothing is written without
`FLOTILLA_PLAN_FILE`).
