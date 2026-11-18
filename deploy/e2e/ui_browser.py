"""The UI in a real browser (ADR-0014): Chromium on the UI network, through a TLS front (Caddy, as the installation
runs it) to flotillad's UI listener. run.sh starts both:

  docker run --rm --network e2e-ui -v deploy/e2e:/e2e:ro local/flotilla-e2e-browser:<tag> \
      python3 /e2e/ui_browser.py ADMIN_TOKEN

What only a browser shows: the Secure, SameSite=Strict session cookie is kept and sent back, forms post
with their CSRF field and follow the redirect, the CSP blocks nothing the pages use, the self-hosted
fonts load, a phone-width page does not scroll sideways, and the routine page's version forms (promote, roll
back, retire) work. Prints one PASS or FAIL line per check;
exits 1 on any FAIL.
"""
import re
import sys

from playwright.sync_api import sync_playwright

NAME, FRONT = "flotilla.e2e", "flotilla-e2e-caddy"
BASE = f"https://{NAME}"
ROUTINE = "platform/heartbeat"
admin = sys.argv[1]
bad = 0


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
    page.click("button[type=submit]")
    page.wait_for_load_state()
    check(page.url == BASE + "/" and "ada" in page.content(), f"signed in, the home page ({page.url})")
    cookie = next((c for c in ctx.cookies() if c["name"] == "__Host-flotilla"), None)
    check(cookie is not None and cookie["httpOnly"] and cookie["secure"] and cookie["sameSite"] == "Strict",
          "the browser keeps the session cookie (HttpOnly, Secure, SameSite=Strict)")

    # Routine versions (docs/wip/v2-design.md section 1) on e2e/versioned's page, which run.sh's versions sections
    # leave with v3 live, v1 previous, v5 and v6 shadows: the table and every form at phone widths (360 to 430 px:
    # no sideways scroll), then the operator's forms in a real browser: promote, roll back, retire.
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

    page.goto(VER)
    check("shadow: not run yet" in vrow(5).inner_text() and vrow(3).locator(".chip", has_text="live").count() == 1,
          "the page shows v3 live and v5 a shadow that has not run")
    vrow(5).locator("summary", has_text="Promote").click()
    pbtn = vrow(5).locator("form[action$='/versions/5/promote'] button[type=submit]")
    check(pbtn.is_disabled(), "Promote waits for a reason (flotilla.js runs under the CSP)")
    pform = vrow(5).locator("form[action$='/versions/5/promote']")
    pform.locator("textarea[name=reason]").fill("e2e: promoted in a browser")
    pform.locator("select[name=autonomy]").select_option("propose")
    pform.locator("textarea[name=gates]").fill("replay-corpus=pass@e2e")
    pform.locator("input[name=without_pairs]").check()
    pbtn.click()
    page.wait_for_load_state()
    check(page.locator(".flash--ok").count() == 1 and vrow(5).locator(".chip", has_text="live").count() == 1
          and vrow(3).locator(".chip", has_text="previous").count() == 1 and "gate replay-corpus pass" in vrow(5).inner_text(),
          "promote v5 from its form: live, v3 previous, the gate result and who and why kept on the row")
    vrow(5).locator("summary", has_text="Roll back").click()
    rbtn = vrow(5).locator("form[action$='/rollback'] button[type=submit]")
    check(rbtn.is_disabled(), "Roll back waits for a reason")
    vrow(5).locator("form[action$='/rollback'] textarea[name=reason]").fill("e2e: rolled back in a browser")
    rbtn.click()
    page.wait_for_load_state()
    check(page.locator(".flash--ok").count() == 1 and vrow(3).locator(".chip", has_text="live").count() == 1
          and "rolled back at" in vrow(5).inner_text() and "e2e: rolled back in a browser" in vrow(5).inner_text(),
          "roll back from its form: v3 live again, v5 retired as rolled back with who and why")
    vrow(6).locator("summary", has_text="Retire").click()
    xbtn = vrow(6).locator("form[action$='/versions/6/retire'] button[type=submit]")
    check(xbtn.is_disabled(), "Retire waits for a reason")
    vrow(6).locator("form[action$='/versions/6/retire'] textarea[name=reason]").fill("e2e: retired in a browser")
    xbtn.click()
    page.wait_for_load_state()
    check(page.locator(".flash--ok").count() == 1 and vrow(6).locator(".chip", has_text="retired").count() == 1,
          "retire a shadow from its form")
    check(page.locator("#versions summary").count() == 0, "with no shadow and no previous version left, no form is offered")

    # the routine's block on its fleet's workflow: pause, then resume with a reason (the Resume… details;
    # flotilla.js keeps its button disabled until the reason has text)
    page.goto(BASE + "/fleets/platform?sel=" + ROUTINE)
    sel = page.locator("aside.selaside")
    sel.locator("form[action$='/pause'] button").click()
    page.wait_for_load_state()
    check(page.locator("aside.selaside summary", has_text="Resume").count() == 1, "pause from the fleet page (a form with its CSRF field)")
    page.locator("aside.selaside summary", has_text="Resume").click()
    check(page.locator("aside.selaside form[action$='/resume'] button").is_disabled(), "Resume waits for a reason (flotilla.js runs under the CSP)")
    page.locator("aside.selaside form[action$='/resume'] textarea[name=reason]").fill("e2e: resumed in a browser")
    page.locator("aside.selaside form[action$='/resume'] button").click()
    page.wait_for_load_state()
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
    page.click("form[action='/signout'] button")
    page.wait_for_load_state()
    page.goto(BASE + "/")
    check(page.url.startswith(BASE + "/signin"), "sign out ends the session")
    browser.close()
sys.exit(1 if bad else 0)
