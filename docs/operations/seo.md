# The seo fleet

The digital marketing fleet ([RFC-0012](../rfc/RFC-0012-seo-fleet.md)). Today one routine,
`seo/site-check`, on the `marketing` worker on the mini: daily, code only, read only, in shadow. Each
failed check opens a `seo_live_regression` case; nothing goes out.

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

A new site: an entry here, the host in `egress-seo-acl.yaml`, its Umami website moved into the
`seo-read` team, and the service account added to its Search Console property.

## What a finding looks like

Target `site:<check>/<host><path>`, e.g. `site:canonical/airandspacehistory.com/`. The checks:
`robots`, `robots-<bot>`, `sitemap`, `status`, `redirect`, `html`, `noindex`, `title`, `description`,
`canonical`, `json-ld`, `hreflang`, `retired`. The case's data carries the URL and a `measure`
(the value found and the one expected). The case clears when the check passes again.

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
       www.markodragoljevic.com, markodragoljevic.com, airandspacehistory.com, www.airandspacehistory.com]
   ```

4. **`FLOTILLA_HOME/.env`:** add `seo` to `COMPOSE_PROFILES`.
5. Deploy as usual (`deploy/mini/deploy.sh`): `secrets.sh` fills the worker's volume.

## Credentials for the next routines (made 2026-11-15, not yet mounted)

- Umami: user `flotilla-seo`, view-only, in team `seo-read`, which owns the sites' websites;
  `~/flotilla/marketing/umami.env` on the mini.
- Search Console: service account `flotilla-seo@flotilla-seo.iam.gserviceaccount.com`, a restricted
  user on the 5 properties; its key `~/flotilla/marketing/gsc.json` on the mini. The token is an RS256
  JWT signed with `openssl` and exchanged at `oauth2.googleapis.com`.

## Run it by hand

`docker exec -i -e FLOTILLA_TOKEN_FILE=/dev/stdin flotillad /usr/local/bin/flotilla run seo/site-check
< ~/flotilla/admin-marko.token`, then `flotilla runs seo/site-check`. Outside flotilla, the script
alone: `python3 seo/site_check.py` (one line per site, one per finding; nothing is written without
`FLOTILLA_PLAN_FILE`).
