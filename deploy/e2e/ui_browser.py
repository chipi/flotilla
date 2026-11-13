"""The UI in a real browser (ADR-0014): Chromium on the UI network, through a TLS front (Caddy, as the homelab
runs it) to flotillad's UI listener. run.sh starts both:

  docker run --rm --network e2e-ui -v deploy/e2e:/e2e:ro local/flotilla-e2e-browser:<tag> \
      python3 /e2e/ui_browser.py ADMIN_TOKEN

What only a browser shows: the Secure, SameSite=Strict session cookie is kept and sent back, forms post
with their CSRF field and follow the redirect, the CSP blocks nothing the pages use, the self-hosted
fonts load, and a phone-width page does not scroll sideways. Prints one PASS or FAIL line per check;
exits 1 on any FAIL.
"""
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

    page.goto(BASE + "/fleets/platform")
    card = page.locator("section.routine", has_text=ROUTINE)
    card.locator("form[action$='/pause'] button").click()
    page.wait_for_load_state()
    check(card.locator(".chip", has_text="paused").count() == 1, "pause from the fleet page (a form with its CSRF field)")
    card.locator("form[action$='/resume'] input[name=reason]").fill("e2e: resumed in a browser")
    card.locator("form[action$='/resume'] button").click()
    page.wait_for_load_state()
    check(card.locator(".chip", has_text="paused").count() == 0 and page.locator(".flash--ok").count() == 1,
          "resume with a reason from the fleet page")

    for path in ["/", "/runs", "/fleets", "/fleets/platform", "/autonomy", "/history", "/users"]:
        page.goto(BASE + path)
        page.evaluate("document.fonts.ready.then(() => 1)")
        wide = page.evaluate("document.documentElement.scrollWidth")
        check(wide <= 390, f"{path} at 390px: no sideways scroll ({wide})")
    faces = page.evaluate("[...document.fonts].filter(f => f.status === 'loaded').map(f => f.family.replace(/\"/g, ''))")
    check("Inter" in faces and "JetBrains Mono" in faces, f"the self-hosted fonts load ({sorted(set(faces))})")
    check(not problems, f"no console errors or CSP violations ({problems[:3]})")

    page.click("form.nav__signout button")
    page.wait_for_load_state()
    page.goto(BASE + "/")
    check(page.url.startswith(BASE + "/signin"), "sign out ends the session")
    browser.close()
sys.exit(1 if bad else 0)
