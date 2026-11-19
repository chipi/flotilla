"""The UI in a real browser (ADR-0014): Chromium on the UI network, through a TLS front (Caddy, as the installation
runs it) to flotillad's UI listener. run.sh starts both:

  docker run --rm --network e2e-ui -v deploy/e2e:/e2e:ro local/flotilla-e2e-browser:<tag> \
      python3 /e2e/ui_browser.py ADMIN_TOKEN

What only a browser shows: the Secure, SameSite=Strict session cookie is kept and sent back, forms post
with their CSRF field and follow the redirect, the CSP blocks nothing the pages use, the self-hosted
fonts load, a phone-width page does not scroll sideways, and the routine page's version forms (promote, roll
back, retire) work. Every click that submits a form waits for the navigation it causes and for the flash on the page
it lands on, never for "the load state", which is already settled before a form's redirect begins. Prints one PASS or
FAIL line per check; exits 1 on any FAIL.
"""
import re
import sys

from playwright.sync_api import sync_playwright

NAME, FRONT = "flotilla.e2e", "flotilla-e2e-caddy"
BASE = f"https://{NAME}"
ROUTINE = "platform/heartbeat"
admin = sys.argv[1]
bad = 0


def submit(page, click):
    """Click something that posts a form, and return once the page the redirect lands on has shown its flash."""
    with page.expect_navigation():
        click()
    page.wait_for_selector(".flash--ok, .flash--err", timeout=15000)


def check(cond, what):
    global bad
    print(("PASS " if cond else "FAIL ") + what)
    bad += 0 if cond else 1


with sync_playwright() as pw:
    # The name resolves to the TLS front; its certificate is from Caddy's own CA, which this browser does
    # not know, hence ignore_https_errors. Everything else is as a person's browser sees it.
    browser = pw.chromium.launch(args=[f"--host-resolver-rules=MAP {NAME} {FRONT}"])
    ctx = browser.new_context(viewport={"width": 390, "height": 844}, ignore_https_errors=True)
    page = ctx.new_page()
    problems = []
    page.on("console", lambda m: m.type == "error" and problems.append(m.text))
    page.on("pageerror", lambda e: problems.append(str(e)))

    page.goto(BASE + "/")
    check(page.url.startswith(BASE + "/signin"), f"without a session, / goes to sign in ({page.url})")
    page.fill("input[name=token]", admin)
    with page.expect_navigation():
        page.click("button[type=submit]")
    page.wait_for_selector("body")
    check(page.url == BASE + "/" and "ada" in page.content(), f"signed in, the home page ({page.url})")
    cookie = next((c for c in ctx.cookies() if c["name"] == "__Host-flotilla"), None)
    check(cookie is not None and cookie["httpOnly"] and cookie["secure"] and cookie["sameSite"] == "Strict",
          "the browser keeps the session cookie (HttpOnly, Secure, SameSite=Strict)")

    # Routine versions (docs/wip/v2-design.md section 1) on e2e/versioned's page: the table and every form at phone widths
    # (360 to 430 px: no sideways scroll), then the operator's forms in a real browser: promote, roll back, retire.
    # The state is read from the page, not assumed from what run.sh left: it needs a live version and two shadows, takes
    # the lower shadow to promote (so the live one becomes the previous one the roll back returns to) and the other to
    # retire, and says so when the page does not offer them.
    VER = BASE + "/fleets/e2e/versioned"
    for w in (360, 390, 430):
        page.set_viewport_size({"width": w, "height": 844})
        for path in ("/fleets/e2e/versioned", "/fleets/platform/heartbeat"):
            page.goto(BASE + path)
            page.evaluate("document.fonts.ready.then(() => 1)")
            wide = page.evaluate("document.documentElement.scrollWidth")
            check(wide <= w, f"{path} at {w}px: no sideways scroll ({wide})")
        page.goto(VER)
        forms = page.locator("#versions details > summary")
        for i in range(forms.count()):
            forms.nth(i).click()
        wide = page.evaluate("document.documentElement.scrollWidth")
        cells = page.evaluate("[...document.querySelectorAll('#versions td')].filter(td => td.getBoundingClientRect().right > window.innerWidth + 1).length")
        check(forms.count() >= 3 and wide <= w and cells == 0,
              f"the versions table with {forms.count()} forms open at {w}px: no sideways scroll, no cell out of the page ({wide}, {cells} cells)")
    page.set_viewport_size({"width": 390, "height": 844})

    def vrow(n):
        return page.locator("#versions tbody tr").filter(has=page.locator("td[data-label=Version]", has_text=f"v{n}"))

    def numbers(chip, label=""):
        out = []
        for i in range(page.locator("#versions tbody tr").count()):
            row = page.locator("#versions tbody tr").nth(i)
            if row.locator(".chip", has_text=chip).count() == 1 and label in row.inner_text():
                out.append(int(row.locator("td[data-label=Version]").inner_text().strip().lstrip("v")))
        return out

    page.goto(VER)
    live, shadows = numbers("live"), numbers("shadow", "shadow: runs beside")
    check(len(live) == 1 and len(shadows) >= 2,
          f"e2e/versioned offers one live version and two shadows to work with (live {live}, shadows {shadows})")
    if len(live) != 1 or len(shadows) < 2:
        browser.close()
        sys.exit(1)
    cur, first, second = live[0], shadows[0], shadows[1]
    check("shadow: runs beside" in vrow(first).inner_text(), f"the page shows v{cur} live and v{first} a shadow beside it")
    vrow(first).locator("summary", has_text="Promote").click()
    pbtn = vrow(first).locator(f"form[action$='/versions/{first}/promote'] button[type=submit]")
    check(pbtn.is_disabled(), "Promote waits for a reason (flotilla.js runs under the CSP)")
    pform = vrow(first).locator(f"form[action$='/versions/{first}/promote']")
    pform.locator("textarea[name=reason]").fill("e2e: promoted in a browser")
    pform.locator("select[name=autonomy]").select_option("propose")
    pform.locator("textarea[name=gates]").fill("replay-corpus=pass@e2e")
    pform.locator("input[name=without_pairs]").check()
    submit(page, pbtn.click)
    check(page.locator(".flash--ok").count() == 1 and vrow(first).locator(".chip", has_text="live").count() == 1
          and vrow(cur).locator(".chip", has_text="previous").count() == 1 and "gate replay-corpus pass" in vrow(first).inner_text(),
          f"promote v{first} from its form: live, v{cur} previous, the gate result and who and why kept on the row")
    vrow(first).locator("summary", has_text="Roll back").click()
    rbtn = vrow(first).locator("form[action$='/rollback'] button[type=submit]")
    check(rbtn.is_disabled(), "Roll back waits for a reason")
    vrow(first).locator("form[action$='/rollback'] textarea[name=reason]").fill("e2e: rolled back in a browser")
    submit(page, rbtn.click)
    check(page.locator(".flash--ok").count() == 1 and vrow(cur).locator(".chip", has_text="live").count() == 1
          and "rolled back at" in vrow(first).inner_text() and "e2e: rolled back in a browser" in vrow(first).inner_text(),
          f"roll back from its form: v{cur} live again, v{first} retired as rolled back with who and why")
    vrow(second).locator("summary", has_text="Retire").click()
    xbtn = vrow(second).locator(f"form[action$='/versions/{second}/retire'] button[type=submit]")
    check(xbtn.is_disabled(), "Retire waits for a reason")
    vrow(second).locator(f"form[action$='/versions/{second}/retire'] textarea[name=reason]").fill("e2e: retired in a browser")
    submit(page, xbtn.click)
    check(page.locator(".flash--ok").count() == 1 and vrow(second).locator(".chip", has_text="retired").count() == 1,
          "retire a shadow from its form")
    left = numbers("shadow", "shadow: runs beside")
    check(left == shadows[2:] and (bool(left) or page.locator("#versions summary").count() == 0),
          f"the shadows left are the ones not touched ({left}); with none left and no previous version, no form is offered")

    # A shadow version's pairs (docs/wip/v2-design.md section 4, step 2), on e2e/shadow-monitor's page: the list and one pair opened from it,
    # at phone widths (360 to 430 px: no sideways scroll), and a tap on a row leads to the two sides.
    PAIRS = BASE + "/fleets/e2e/shadow-monitor?pairs=2#pairs"
    for width in (360, 390, 430):
        page.set_viewport_size({"width": width, "height": 844})
        page.goto(PAIRS)
        page.evaluate("document.fonts.ready.then(() => 1)")
        rows = page.locator("#pairs .pair")
        wide = page.evaluate("document.documentElement.scrollWidth")
        check(rows.count() >= 2 and wide <= width, f"the pairs list at {width}px: {rows.count()} rows, no sideways scroll ({wide})")
    check("shadow vs" in page.locator("#pairs h2").inner_text() and "pairs" in page.locator(".pairs__count").inner_text()
          and page.locator(".pairs__filter a").count() >= 3, "the pairs header carries the counts and the filter (all, differ, unpaired)")
    with page.expect_navigation():
        page.locator("#pairs .pair").first.click()
    for width in (360, 430):
        page.set_viewport_size({"width": width, "height": 844})
        page.evaluate("document.fonts.ready.then(() => 1)")
        wide = page.evaluate("document.documentElement.scrollWidth")
        check(page.locator(".sides .side").count() == 2 and wide <= width, f"a tapped pair opens its two sides at {width}px, no sideways scroll ({wide})")
    page.set_viewport_size({"width": 390, "height": 844})

    # the routine's block on its fleet's workflow: pause, then resume with a reason (the Resume… details;
    # flotilla.js keeps its button disabled until the reason has text)
    page.goto(BASE + "/fleets/platform?sel=" + ROUTINE)
    sel = page.locator("aside.selaside")
    submit(page, sel.locator("form[action$='/pause'] button").click)
    check(page.locator("aside.selaside summary", has_text="Resume").count() == 1, "pause from the fleet page (a form with its CSRF field)")
    page.locator("aside.selaside summary", has_text="Resume").click()
    check(page.locator("aside.selaside form[action$='/resume'] button").is_disabled(), "Resume waits for a reason (flotilla.js runs under the CSP)")
    page.locator("aside.selaside form[action$='/resume'] textarea[name=reason]").fill("e2e: resumed in a browser")
    submit(page, page.locator("aside.selaside form[action$='/resume'] button").click)
    check(page.locator("aside.selaside form[action$='/pause']").count() == 1 and page.locator(".flash--ok").count() == 1,
          "resume with a reason from the fleet page")

    # the backup chain's case (run.sh's cases section ran first; the business examples close theirs too): its page, reached from the list
    page.goto(BASE + "/cases?status=closed")
    # the closed one: flotillad restarts during the e2e and its monitor runs again, opening a newer case
    link = page.locator("tr", has_text="plan recorded").filter(has_text=re.compile("backup")).locator("a[href^='/cases/case-']").first
    case_path = link.get_attribute("href") if link.count() else ""
    check(case_path != "", "the cases page lists the chain's case")
    for path in ["/", "/approvals", "/cases", "/runs", "/fleets", "/fleets/platform", "/fleets/platform?tab=stats", "/hosts",
                 "/autonomy", "/models", "/knowledge", "/brief", "/brief/archive", "/history", "/account", "/users"] + ([case_path] if case_path else []):
        page.goto(BASE + path)
        page.evaluate("document.fonts.ready.then(() => 1)")
        wide = page.evaluate("document.documentElement.scrollWidth")
        check(wide <= 390, f"{path} at 390px: no sideways scroll ({wide})")
    if case_path:
        page.goto(BASE + case_path)
        body = page.content()
        missing = [w for w in ("backup.rerun", "would have been", "plan_recorded") if w not in body]
        check(not missing, f"the case page shows the plan, what it would have been, and the close (missing: {missing})")
    faces = page.evaluate("[...document.fonts].filter(f => f.status === 'loaded').map(f => f.family.replace(/\"/g, ''))")
    check("Inter" in faces and "JetBrains Mono" in faces, f"the self-hosted fonts load ({sorted(set(faces))})")
    check(not problems, f"no console errors or CSP violations ({problems[:3]})")

    # G1: the heartbeat's model call is on its run (the calls file is in /tmp: the worker's HOME is
    # read-only, and the calls were lost there)
    page.goto(BASE + "/runs?routine=platform%2Fheartbeat")
    hb = page.locator('a[href^="/runs/platform-heartbeat-"]')
    found = False
    for i in range(min(hb.count(), 5)):
        page.goto(BASE + hb.nth(i).get_attribute("href"))
        if "triage-flash" in page.locator("#calls-h").locator("xpath=..").inner_text():
            found = True
            break
        page.goto(BASE + "/runs?routine=platform%2Fheartbeat")
        hb = page.locator('a[href^="/runs/platform-heartbeat-"]')
    check(found, "a heartbeat run shows its model call (G1: recorded from the worker)")
    page.goto(BASE + "/account")
    with page.expect_navigation():
        page.click("form[action='/signout'] button")
    page.goto(BASE + "/")
    check(page.url.startswith(BASE + "/signin"), "sign out ends the session")
    browser.close()
sys.exit(1 if bad else 0)
