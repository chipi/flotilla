// bugfix/github_app.js: a GitHub App JWT (RS256), for bugfix/open_pr.py to exchange for an installation
// token. Python's standard library has no RSA signing; Node's has. Reads the App's private key from the
// file named by argv[2] and the App id from argv[3]; writes the JWT (valid 9 minutes) to stdout.
//   node bugfix/github_app.js /run/flotilla/github-app.pem 123456
"use strict";
const crypto = require("node:crypto");
const fs = require("node:fs");

const [keyFile, appId] = process.argv.slice(2);
if (!keyFile || !/^[0-9]+$/.test(appId || "")) {
  process.stderr.write("usage: github_app.js KEY_FILE APP_ID\n");
  process.exit(2);
}
const b64 = (s) => Buffer.from(s).toString("base64url");
const now = Math.floor(Date.now() / 1000);
const head = b64(JSON.stringify({ alg: "RS256", typ: "JWT" }));
// iat 60 s in the past: GitHub's advice for clock drift; exp at most 10 minutes after iat
const body = b64(JSON.stringify({ iat: now - 60, exp: now + 9 * 60, iss: appId }));
const sig = crypto.sign("RSA-SHA256", Buffer.from(head + "." + body), fs.readFileSync(keyFile)).toString("base64url");
process.stdout.write(head + "." + body + "." + sig);
