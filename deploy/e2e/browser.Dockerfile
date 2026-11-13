# The e2e browser check's image (ui_browser.py): Microsoft's Playwright image ships Chromium but not the
# Python package, and the UI network has no route out, so the package is installed here, at build time.
# The package version must match the image's browsers (v1.65.1).
FROM mcr.microsoft.com/playwright/python@sha256:0ab41a37431309d72b7a26f40ef1fdb464a62740196b2143965a2f59dc090409
RUN pip install --no-cache-dir --break-system-packages playwright==1.65.1
